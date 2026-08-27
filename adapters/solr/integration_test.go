//go:build integration

package main_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/probavi/probavi/internal/adapter"
	"github.com/probavi/probavi/internal/capabilities"
	"github.com/probavi/probavi/internal/sandbox"
	"github.com/probavi/probavi/internal/sandbox/docker"
)

// verifiedImage is the engine image this run restores from: the
// manifest's baseline, or the version-matrix job's PROBAVI_IT_IMAGE when
// it names one the manifest already lists. The manifest and this suite
// read the same values, so docs/capabilities.json can never claim an
// engine version CI does not actually restore from (docs/capabilities.md
// §1, docs/engine-versions.md §2).
func verifiedImage(t *testing.T) string {
	t.Helper()
	m, err := capabilities.LoadAdapterManifest(".")
	if err != nil {
		t.Fatalf("load adapter manifest: %v", err)
	}
	image, err := m.SandboxImage(os.Getenv("PROBAVI_IT_IMAGE"))
	if err != nil {
		t.Fatalf("adapter manifest: %v", err)
	}
	return image
}

// sandboxParams returns the documented drill-config sandbox params.
//
// There is no command override: the official image starts Solr itself, in
// SolrCloud mode with an embedded ZooKeeper, and it serves under
// `--network none` in about two seconds (measured). This is the first
// adapter in the catalog whose engine needs neither an idle sandbox nor a
// start step.
func sandboxParams(t *testing.T) map[string]string {
	return map[string]string{"image": verifiedImage(t), "memory": "2g"}
}

const documents = 250

// TestEndToEndRestoreDrill proves the twentieth engine through the
// adapter the core actually runs: a backup taken from one server is
// restored into a sandbox that has never seen it, and the checks read
// what the backup held.
func TestEndToEndRestoreDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	provider := docker.New(nil)
	// The artifact's own directory name is deliberately not the
	// collection name: the adapter reads the collection out of the
	// backup's layout, and the transfer renames the directory anyway.
	backup := makeBackup(t, ctx, provider, "orders", "nightly-2026-08-27")

	runner, err := adapter.New("solr", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	sbx := freshSandbox(t, ctx, provider)
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "solr_backup", Path: backup},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if res.Connection.Database != "orders" {
		t.Errorf("connection.database = %q, want the collection the backup holds", res.Connection.Database)
	}
	if res.SourceIdentity.CreatedAt == nil {
		t.Error("created_at is nil — the backup records its own start time")
	} else if _, perr := time.Parse(time.RFC3339Nano, *res.SourceIdentity.CreatedAt); perr != nil {
		t.Errorf("created_at = %q, want the engine's own RFC 3339 instant", *res.SourceIdentity.CreatedAt)
	}
	if res.Timings.RestoreSeconds <= 0 {
		t.Errorf("restore_seconds = %v, want a measurement", res.Timings.RestoreSeconds)
	}

	// Checks: Solr queries, in the dialect the runner declares.
	for _, tc := range []struct{ name, query, want string }{
		{"every document came back", "q=*:*&rows=0", fmt.Sprint(documents)},
		{"a filter answers a count", "q=n:[0 TO 9]&rows=0", "10"},
		{"a query returns the rows it asked for", "q=id:doc-7&rows=1&fl=id", "doc-7"},
		{"a query matching nothing answers zero", "q=id:nothing-here&rows=0", "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runCheck(t, ctx, sbx, res.Connection.Database, tc.query)
			if got != tc.want {
				t.Errorf("check %q = %q, want %q", tc.query, got, tc.want)
			}
		})
	}

	t.Run("healthcheck agrees", func(t *testing.T) {
		health, err := runner.Healthcheck(ctx, &res.Connection, res.State, sbx)
		if err != nil {
			t.Fatalf("healthcheck: %v", err)
		}
		if !health.Healthy {
			t.Errorf("healthcheck = %+v, want healthy", health)
		}
	})
}

// TestBrokenBackupVerdicts proves the adapter blames the backup only when
// the backup is what is wrong.
func TestBrokenBackupVerdicts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	buildAdapterOnPath(t, ctx)
	provider := docker.New(nil)
	good := makeBackup(t, ctx, provider, "orders", "good")

	tests := map[string]struct {
		mutate func(t *testing.T, dir string)
		want   string
	}{
		"an index the engine cannot read": {
			mutate: func(t *testing.T, dir string) {
				matches, _ := filepath.Glob(filepath.Join(dir, "orders", "index", "*"))
				if len(matches) == 0 {
					t.Fatal("fixture has no index files to damage")
				}
				for _, m := range matches {
					if err := os.WriteFile(m, []byte("not an index"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			},
			want: "source_corrupt",
		},
		"a backup with its metadata removed": {
			mutate: func(t *testing.T, dir string) {
				if err := os.RemoveAll(filepath.Join(dir, "orders", "shard_backup_metadata")); err != nil {
					t.Fatal(err)
				}
			},
			want: "source_corrupt",
		},
	}
	runner, err := adapter.New("solr", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			broken := copyTree(t, good, filepath.Join(t.TempDir(), "broken"))
			tt.mutate(t, broken)
			sbx := freshSandbox(t, ctx, provider)
			_, err := runner.Provision(ctx, &adapter.ProvisionRequest{
				Source:  adapter.ProvisionSource{Kind: "solr_backup", Path: broken},
				Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
			}, sbx)
			var aerr *adapter.Error
			if err == nil || !errors.As(err, &aerr) {
				t.Fatalf("provision error = %v, want an adapter verdict", err)
			}
			if aerr.Code != tt.want {
				t.Errorf("code = %s (%s), want %s", aerr.Code, aerr.Message, tt.want)
			}
		})
	}
}

// TestABackupThatDeletesItsOwnDocumentsIsRefused is the fence, proven
// against the engine rather than asserted.
//
// A backup carries its collection's configset, so a collection that uses
// DocExpirationUpdateProcessorFactory brings the deleter with it. Measured
// on Solr 10: a backup taken while the documents were live restores with
// status 0 and is empty seconds later. Nothing can be suspended — the
// setting is the operator's own configuration — so the drill is refused
// before a byte is transferred.
func TestABackupThatDeletesItsOwnDocumentsIsRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	buildAdapterOnPath(t, ctx)
	provider := docker.New(nil)

	backup := makeExpiringBackup(t, ctx, provider)
	runner, err := adapter.New("solr", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	sbx := freshSandbox(t, ctx, provider)
	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "solr_backup", Path: backup},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) {
		t.Fatalf("provision error = %v, want the drill refused", err)
	}
	if aerr.Code != "unsupported_source" {
		t.Errorf("code = %s (%s), want unsupported_source", aerr.Code, aerr.Message)
	}
	for _, want := range []string{"DocExpirationUpdateProcessorFactory", "empty"} {
		if !strings.Contains(aerr.Message, want) {
			t.Errorf("message = %q, want it to carry %q", aerr.Message, want)
		}
	}
}

// makeBackup seeds a server, indexes documents, takes a Collections API
// backup and copies it out under the given directory name.
func makeBackup(t *testing.T, ctx context.Context, provider *docker.Provider, collection, name string) string {
	t.Helper()
	seed := freshSandbox(t, ctx, provider)
	awaitSolr(t, ctx, seed)
	mustSolr(t, ctx, seed, "solr", "create", "-c", collection)
	docs := &strings.Builder{}
	docs.WriteString("[")
	for i := range documents {
		if i > 0 {
			docs.WriteString(",")
		}
		fmt.Fprintf(docs, `{"id":"doc-%d","n":%d}`, i, i)
	}
	docs.WriteString("]")
	mustSolr(t, ctx, seed, "curl", "-sf", "-X", "POST",
		"http://127.0.0.1:8983/solr/"+collection+"/update?commit=true",
		"-H", "Content-Type: application/json", "-d", docs.String())
	home := solrHome(t, ctx, seed)
	mustSolr(t, ctx, seed, "mkdir", "-p", home+"/bk")
	mustSolr(t, ctx, seed, "curl", "-sf", "--get", "http://127.0.0.1:8983/solr/admin/collections",
		"--data-urlencode", "action=BACKUP", "--data-urlencode", "name="+name,
		"--data-urlencode", "collection="+collection, "--data-urlencode", "location="+home+"/bk")
	return copyOut(t, ctx, seed, home+"/bk/"+name, filepath.Join(t.TempDir(), name))
}

// makeExpiringBackup builds the artifact the fence exists for: a
// collection whose own configset deletes expired documents.
func makeExpiringBackup(t *testing.T, ctx context.Context, provider *docker.Provider) string {
	t.Helper()
	seed := freshSandbox(t, ctx, provider)
	awaitSolr(t, ctx, seed)
	const script = `set -e
cp -r /opt/solr/server/solr/configsets/_default/conf /tmp/c
sed -i 's#</config>#<updateRequestProcessorChain name="expire" default="true"><processor class="solr.processor.DocExpirationUpdateProcessorFactory"><int name="autoDeletePeriodSeconds">5</int><str name="expirationFieldName">_expire_at_</str></processor><processor class="solr.LogUpdateProcessorFactory"/><processor class="solr.RunUpdateProcessorFactory"/></updateRequestProcessorChain></config>#' /tmp/c/solrconfig.xml
sed -i 's#</schema>#<field name="_expire_at_" type="pdate" indexed="true" stored="true"/></schema>#' /tmp/c/managed-schema.xml
solr zk upconfig -n expcfg -d /tmp/c -z localhost:9983
curl -sf "http://127.0.0.1:8983/solr/admin/collections?action=CREATE&name=ttl&numShards=1&collection.configName=expcfg"
curl -sf -X POST "http://127.0.0.1:8983/solr/ttl/update?commit=true" -H 'Content-Type: application/json' \
  -d '[{"id":"a","_expire_at_":"2099-01-01T00:00:00Z"}]'
mkdir -p "$1/bk"
curl -sf --get http://127.0.0.1:8983/solr/admin/collections --data-urlencode action=BACKUP \
  --data-urlencode name=ttlbk --data-urlencode collection=ttl --data-urlencode "location=$1/bk"`
	home := solrHome(t, ctx, seed)
	mustSolr(t, ctx, seed, "bash", "-c", script, "bash", home)
	return copyOut(t, ctx, seed, home+"/bk/ttlbk", filepath.Join(t.TempDir(), "ttlbk"))
}

func solrHome(t *testing.T, ctx context.Context, sbx *docker.Sandbox) string {
	t.Helper()
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{
		Argv: []string{"sh", "-c", `printf %s "${SOLR_HOME:-/var/solr/data}"`},
	})
	if err != nil || out.ExitCode != 0 {
		t.Fatalf("resolve SOLR_HOME: %v (exit %d)", err, out.ExitCode)
	}
	return strings.TrimSpace(string(out.Stdout))
}

func awaitSolr(t *testing.T, ctx context.Context, sbx *docker.Sandbox) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		out, err := sbx.Exec(ctx, sandbox.ExecRequest{
			Argv: []string{"sh", "-c", `curl -sf -o /dev/null http://127.0.0.1:8983/solr/admin/info/system`},
		})
		if err == nil && out.ExitCode == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("Solr never became ready in the seed sandbox")
		}
		time.Sleep(time.Second)
	}
}

func mustSolr(t *testing.T, ctx context.Context, sbx *docker.Sandbox, argv ...string) {
	t.Helper()
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: argv})
	if err != nil {
		t.Fatalf("exec %v: %v", argv, err)
	}
	if out.ExitCode != 0 {
		t.Fatalf("exec %v: exit %d: %s", argv, out.ExitCode, out.Stderr)
	}
}

// runCheck runs one check through the runner the probe declares, the way
// the core does.
func runCheck(t *testing.T, ctx context.Context, sbx *docker.Sandbox, collection, query string) string {
	t.Helper()
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{
		Argv: []string{"bash", "-c", runnerScriptForTest(t), "bash", collection, query},
	})
	if err != nil {
		t.Fatalf("check %q: %v", query, err)
	}
	if out.ExitCode != 0 {
		t.Fatalf("check %q: exit %d: %s", query, out.ExitCode, out.Stderr)
	}
	return strings.TrimSpace(string(out.Stdout))
}

// runnerScriptForTest reads the runner out of the probe response, so the
// suite exercises the script the adapter actually publishes rather than a
// copy of it.
func runnerScriptForTest(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash("testdata/probe_response.golden"))
	if err != nil {
		t.Fatalf("read probe golden: %v", err)
	}
	const key = `"argv":["bash","-c",`
	i := strings.Index(string(raw), key)
	if i < 0 {
		t.Fatal("probe golden declares no bash runner")
	}
	rest := string(raw)[i+len(key):]
	end := strings.Index(rest, `,"bash"`)
	if end < 0 {
		t.Fatal("probe golden's runner argv has an unexpected shape")
	}
	var script string
	if err := json.Unmarshal([]byte(rest[:end]), &script); err != nil {
		t.Fatalf("decode runner script: %v", err)
	}
	return script
}

func freshSandbox(t *testing.T, ctx context.Context, provider *docker.Provider) *docker.Sandbox {
	t.Helper()
	sbx, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	t.Cleanup(func() {
		if err := sbx.Destroy(context.WithoutCancel(ctx)); err != nil {
			t.Errorf("destroy sandbox: %v", err)
		}
	})
	return sbx
}

func copyOut(t *testing.T, ctx context.Context, sbx *docker.Sandbox, from, to string) string {
	t.Helper()
	cmd := exec.CommandContext(ctx, "docker", "cp", sbx.ID()+":"+from, to)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("copy %s out: %v: %s", from, err, out)
	}
	return to
}

func copyTree(t *testing.T, from, to string) string {
	t.Helper()
	if out, err := exec.Command("cp", "-a", from, to).CombinedOutput(); err != nil {
		t.Fatalf("copy tree: %v: %s", err, out)
	}
	return to
}

func buildAdapterOnPath(t *testing.T, ctx context.Context) {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", filepath.Join(dir, "probavi-adapter-solr"), ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, out)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
