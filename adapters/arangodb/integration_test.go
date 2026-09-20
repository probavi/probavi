//go:build integration

package main_test

import (
	"context"
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

// verifiedImage is the official image this run restores with: the
// manifest's baseline, or the version-matrix job's PROBAVI_IT_IMAGE when
// it names one the manifest already lists (docs/engine-versions.md §2).
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

// sandboxParams is the shape the README documents. The image idles under
// `sleep infinity` and the adapter owns the server; 1 GiB is comfortably
// above the 768 MiB the engine was measured to serve at.
func sandboxParams(image string) map[string]string {
	return map[string]string{
		"image": image, "command": "sleep infinity", "memory": "1GiB",
	}
}

// startScript boots a server the same way the adapter does — including
// the flag that holds the expiry thread back, so a fixture with a TTL
// index keeps its documents long enough to be dumped. Every script here
// is `sh`: this image ships no bash (measured).
const startScript = `nohup sh -c 'arangod --server.authentication false \
  --server.endpoint tcp://127.0.0.1:8529 --database.directory /var/lib/arangodb3 \
  --javascript.app-path /var/lib/arangodb3-apps --ttl.frequency 0 >/tmp/arangod.log 2>&1' \
  >/dev/null 2>&1 &
for i in $(seq 1 60); do
  arangosh --quiet --server.endpoint tcp://127.0.0.1:8529 --server.authentication false \
    --javascript.execute-string 'db._version()' >/dev/null 2>&1 && break
  sleep 2
done
arangosh --quiet --server.endpoint tcp://127.0.0.1:8529 --server.authentication false \
  --javascript.execute-string 'db._version()' >/dev/null`

// seedJS creates the fixture: two ordinary collections and one whose
// documents are already past a TTL the moment they are written.
const seedJS = `db._createDatabase("probavi");
var d = require("@arangodb").db;
d._useDatabase("probavi");
d._create("orders");
for (var i = 1; i <= 500; i++) { d.orders.save({_key: "k" + i, n: i, v: "row" + i}); }
d._create("meta");
d.meta.save({_key: "origin", v: "restored-ok"});
d._create("sessions");
d.sessions.ensureIndex({type: "ttl", fields: ["stamp"], expireAfter: 60});
var past = Math.floor(Date.now() / 1000) - 3600;
for (var j = 1; j <= 200; j++) { d.sessions.save({_key: "s" + j, stamp: past, v: "session" + j}); }
print("seeded");`

var seedScript = startScript + `
arangosh --quiet --server.endpoint tcp://127.0.0.1:8529 --server.authentication false \
  --javascript.execute-string ` + squote(seedJS) + `
arangodump --server.endpoint tcp://127.0.0.1:8529 --server.authentication false \
  --server.database probavi --output-directory /tmp/collect >/dev/null`

func squote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// makeDump seeds a real server and extracts its arangodump output to dest.
func makeDump(t *testing.T, ctx context.Context, provider *docker.Provider, image, dest string) {
	t.Helper()
	seed, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)
	res, err := seed.Exec(ctx, sandbox.ExecRequest{Argv: []string{"sh", "-c", seedScript}})
	if err != nil {
		t.Fatalf("seed exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("seed fixture: exit %d: %s", res.ExitCode, res.Stderr)
	}
	if out, err := exec.CommandContext(ctx, "docker", "cp", seed.ID()+":/tmp/collect", dest).CombinedOutput(); err != nil {
		t.Fatalf("extract fixture: %v: %s", err, out)
	}
}

// TestEndToEndRestoreDrill is the whole point: a real dump of a real
// server, restored into a fresh sandbox, answering the core's own
// generating built-ins through the declared runner.
func TestEndToEndRestoreDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	dump := filepath.Join(t.TempDir(), "collect")
	makeDump(t, ctx, provider, image, dump)

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("arangodb", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "arangodb_dump", Path: dump},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if res.Connection.Database != "probavi" {
		t.Errorf("connection.database = %q, want the database the dump names", res.Connection.Database)
	}
	if res.SourceIdentity.CreatedAt == nil {
		t.Error("dump.json states an instant; created_at is null")
	}
	if res.Timings.RestoreSeconds <= 0 {
		t.Errorf("restore_seconds = %v, want a measurement", res.Timings.RestoreSeconds)
	}

	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	// The three statements the core composes, driven exactly as it drives
	// them — which is what proves the dialect the probe promises.
	assertRunner(t, ctx, sbx, probe, res.Connection.Database,
		`SELECT count(*) FROM "orders"`, "500")
	assertRunner(t, ctx, sbx, probe, res.Connection.Database,
		`SELECT count(*) FROM "meta"`, "1")
	// table_exists must succeed and print nothing: no restored document
	// belongs on stdout for a probe about a collection's existence.
	assertRunner(t, ctx, sbx, probe, res.Connection.Database,
		`SELECT count(*) FROM "orders" WHERE 1=0`, "")
	// And the operator's own AQL passes through.
	assertRunner(t, ctx, sbx, probe, res.Connection.Database,
		`FOR d IN meta RETURN d.v`, "restored-ok")
}

// TestExpiredDocumentsSurviveTheDrill is this adapter's half of issue
// #166. The fixture's documents are an hour past a 60-second TTL when
// they are dumped, and the engine's background thread would delete every
// one of them within its default 30-second period. The adapter starts the
// server with that thread turned off, so the drill proves what the backup
// held.
//
// The wait is the test: a drill that merely restored quickly would pass
// this by accident.
func TestExpiredDocumentsSurviveTheDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	dump := filepath.Join(t.TempDir(), "collect")
	makeDump(t, ctx, provider, image, dump)

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("arangodb", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "arangodb_dump", Path: dump},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}

	// Past the default TTL period, twice over.
	select {
	case <-ctx.Done():
		t.Fatal("cancelled while waiting past the expiry thread's period")
	case <-time.After(70 * time.Second):
	}

	assertRunner(t, ctx, sbx, probe, res.Connection.Database,
		`SELECT count(*) FROM "sessions"`, "200")

	// Suspend, never rewrite: the index the operator declared is still
	// there, so a check that reads it sees what the backup held.
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{"sh", "-c",
		`arangosh --quiet --server.endpoint tcp://127.0.0.1:8529 --server.authentication false ` +
			`--server.database probavi --javascript.execute-string ` +
			`'print(db.sessions.getIndexes().filter(function (x) { return x.type === "ttl"; })[0].expireAfter)'`}})
	if err != nil || out.ExitCode != 0 {
		t.Fatalf("read the ttl index: %v (exit %d) %s", err, out.ExitCode, out.Stderr)
	}
	if got := strings.TrimSpace(string(out.Stdout)); got != "60" {
		t.Errorf("expireAfter = %q, want the 60 the operator declared — the drill must suspend, not rewrite", got)
	}
}

// TestAMissingDataFileIsRefusedBeforeTransfer proves the completeness
// gate against a real dump: dump.json carries no list of collections, so
// the check is that every collection has both halves.
func TestAMissingDataFileIsRefusedBeforeTransfer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	dump := filepath.Join(t.TempDir(), "collect")
	makeDump(t, ctx, provider, image, dump)

	matches, err := filepath.Glob(filepath.Join(dump, "orders_*.data.json.gz"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("fixture holds no data file to remove: %v", err)
	}
	if err := os.Remove(matches[0]); err != nil {
		t.Fatalf("remove data file: %v", err)
	}

	runner, err := adapter.New("arangodb", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "arangodb_dump", Path: dump},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) || aerr.Code != "source_corrupt" {
		t.Fatalf("provision error = %v, want source_corrupt for an incomplete dump", err)
	}
	if !strings.Contains(aerr.Message, "no data file") {
		t.Errorf("message = %q, want it to name the missing half", aerr.Message)
	}
}

// assertRunner drives the adapter's declared runner the way the core
// does — templates substituted, output read undecorated.
func assertRunner(t *testing.T, ctx context.Context, sbx *docker.Sandbox,
	probe *adapter.ProbeResult, database, checkText, want string) {
	t.Helper()
	argv := make([]string, 0, len(probe.SQLRunner.Argv))
	for _, a := range probe.SQLRunner.Argv {
		a = strings.ReplaceAll(a, "{{database}}", database)
		argv = append(argv, strings.ReplaceAll(a, "{{sql}}", checkText))
	}
	env := map[string]string{}
	for k, v := range probe.SQLRunner.Env {
		v = strings.ReplaceAll(v, "{{database}}", database)
		env[k] = strings.ReplaceAll(v, "{{sql}}", checkText)
	}
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: argv, Env: env})
	if err != nil {
		t.Fatalf("runner exec: %v", err)
	}
	got := strings.TrimRight(string(out.Stdout), "\n")
	if out.ExitCode != 0 || got != want {
		t.Fatalf("check %q = %q (exit %d, stderr %s), want %q",
			checkText, got, out.ExitCode, out.Stderr, want)
	}
}

func buildAdapterOnPath(t *testing.T, ctx context.Context) {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "probavi-adapter-arangodb")
	cmd := exec.CommandContext(ctx, "go", "build", "-o", out, ".")
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, combined)
	}
	t.Setenv("PATH", fmt.Sprintf("%s%c%s", dir, os.PathListSeparator, os.Getenv("PATH")))
}

func destroy(t *testing.T, sbx *docker.Sandbox) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := sbx.Destroy(ctx); err != nil {
		t.Errorf("destroy sandbox: %v", err)
	}
}
