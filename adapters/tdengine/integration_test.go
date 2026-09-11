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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/probavi/probavi/internal/adapter"
	"github.com/probavi/probavi/internal/capabilities"
	"github.com/probavi/probavi/internal/sandbox"
	"github.com/probavi/probavi/internal/sandbox/docker"
)

// verifiedImage is the engine image this run restores from: the manifest's
// baseline, or the version-matrix job's PROBAVI_IT_IMAGE when it names one
// the manifest already lists.
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

// sandboxParams are the documented drill-config sandbox params: idle, so
// the adapter starts the engine itself. The image's own entrypoint does
// not always finish (scripts.go), and a drill has no business depending
// on which host it is running on.
func sandboxParams(t *testing.T) map[string]string {
	return map[string]string{"image": verifiedImage(t), "command": "sleep infinity", "memory": "1g"}
}

const (
	documents = 250
	database  = "drill"
)

// TestEndToEndRestoreDrill proves the engine through the adapter the core
// actually runs: a backup taken from one server is restored into a
// sandbox that has never seen it, and the checks read what it held.
func TestEndToEndRestoreDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	provider := docker.New(nil)
	dump := makeDump(t, ctx, provider)

	runner, err := adapter.New("tdengine", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	sbx := freshSandbox(t, ctx, provider)
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		// The inner payload directory, named directly.
		Source:  adapter.ProvisionSource{Kind: "taosdump", Path: innerDumpDir(t, dump)},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if res.SourceIdentity.CreatedAt == nil {
		t.Error("created_at is nil — taosdump records when it started")
	}
	if res.Connection.Database != database {
		t.Errorf("connection.database = %q, want the database the dump holds", res.Connection.Database)
	}
	if res.Timings.RestoreSeconds <= 0 {
		t.Errorf("restore_seconds = %v, want a measurement", res.Timings.RestoreSeconds)
	}

	for _, tc := range []struct{ name, query, want string }{
		{"every row came back", "SELECT count(*) FROM drill.meters", strconv.Itoa(documents)},
		{"the column data is real", "SELECT sum(voltage) FROM drill.meters",
			strconv.Itoa(2 * sumVoltage())},
		{"a filter answers", "SELECT count(*) FROM drill.meters WHERE voltage < 210", "20"},
		{"both subtables came back", "SELECT count(*) FROM information_schema.ins_tables WHERE db_name = 'drill'", "2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := runCheck(t, ctx, sbx, tc.query); got != tc.want {
				t.Errorf("check %q = %q, want %q", tc.query, got, tc.want)
			}
		})
	}

	t.Run("healthcheck agrees", func(t *testing.T) {
		health, err := runner.Healthcheck(ctx, &res.Connection, res.State, sbx)
		if err != nil || !health.Healthy {
			t.Errorf("healthcheck = %+v err=%v", health, err)
		}
	})
	t.Run("teardown is idempotent", func(t *testing.T) {
		for i := range 2 {
			if _, err := runner.Teardown(ctx, res.State, "completed", sbx); err != nil {
				t.Fatalf("teardown %d: %v", i, err)
			}
		}
	})
}

// TestOuterDirectoryRestoresWhole pins the engine-facing half of the
// resolution: `taosdump -i` pointed at the directory `-o` was given exits
// 0 and creates nothing (measured), so the adapter finds the payload
// directory itself and the drill may name either level.
func TestOuterDirectoryRestoresWhole(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	provider := docker.New(nil)
	dump := makeDump(t, ctx, provider)

	runner, err := adapter.New("tdengine", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	sbx := freshSandbox(t, ctx, provider)
	if _, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		// The outer directory, which the tool alone would no-op on.
		Source:  adapter.ProvisionSource{Kind: "taosdump", Path: dump},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx); err != nil {
		t.Fatalf("provision from the outer directory: %v", err)
	}
	if got := runCheck(t, ctx, sbx, "SELECT count(*) FROM drill.meters"); got != strconv.Itoa(documents) {
		t.Errorf("rows = %q, want %d", got, documents)
	}
}

// TestDamagedDumpIsRefused pins the verdict against the engine: taosdump
// restores what it can from a damaged backup, prints a failure line beside
// its summary and exits 0 (measured, 125 of 250 rows). The drill must not
// call that a success.
func TestDamagedDumpIsRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	provider := docker.New(nil)
	dump := makeDump(t, ctx, provider)
	damageOneAvro(t, dump)

	runner, err := adapter.New("tdengine", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	sbx := freshSandbox(t, ctx, provider)
	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "taosdump", Path: dump},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) || aerr.Code != "source_corrupt" {
		t.Fatalf("provision error = %v, want source_corrupt for a damaged backup", err)
	}
}

// TestArchiveRestores covers the kind an operator reaches for when a dump
// travels as one file.
func TestArchiveRestores(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	provider := docker.New(nil)
	dump := makeDump(t, ctx, provider)
	archive := filepath.Join(t.TempDir(), "dump.tar")
	if out, err := exec.CommandContext(ctx, "tar", "-cf", archive,
		"-C", filepath.Dir(dump), filepath.Base(dump)).CombinedOutput(); err != nil {
		t.Fatalf("tar the dump: %v: %s", err, out)
	}

	runner, err := adapter.New("tdengine", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	sbx := freshSandbox(t, ctx, provider)
	if _, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "taosdump_tar", Path: archive},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx); err != nil {
		t.Fatalf("provision from an archive: %v", err)
	}
	if got := runCheck(t, ctx, sbx, "SELECT count(*) FROM drill.meters"); got != strconv.Itoa(documents) {
		t.Errorf("rows = %q, want %d", got, documents)
	}
}

// sumVoltage is what one subtable's voltage column adds up to; the seed
// writes the same series into both.
func sumVoltage() int {
	total := 0
	for i := range documents / 2 {
		total += 200 + i
	}
	return total
}

// makeDump seeds a server and takes a real taosdump backup of it.
func makeDump(t *testing.T, ctx context.Context, provider *docker.Provider) string {
	t.Helper()
	seed := freshSandbox(t, ctx, provider)
	awaitServing(t, ctx, seed)

	var values strings.Builder
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for i := range documents / 2 {
		fmt.Fprintf(&values, " ('%s', %d.5, %d)",
			base.Add(time.Duration(i)*time.Minute).Format("2006-01-02 15:04:05.000"), i, 200+i)
	}
	seedSQL := []string{
		"CREATE DATABASE IF NOT EXISTS " + database,
		"CREATE STABLE " + database + ".meters (ts TIMESTAMP, current FLOAT, voltage INT) TAGS (location BINARY(32), groupid INT)",
		"CREATE TABLE " + database + ".d1 USING " + database + ".meters TAGS ('lab', 1)",
		"CREATE TABLE " + database + ".d2 USING " + database + ".meters TAGS ('lab', 2)",
		"INSERT INTO " + database + ".d1 VALUES" + values.String(),
		"INSERT INTO " + database + ".d2 VALUES" + values.String(),
	}
	for _, statement := range seedSQL {
		if out := runCheck(t, ctx, seed, statement); strings.Contains(out, "error") {
			t.Fatalf("seed %q: %s", statement, out)
		}
	}
	if got := runCheck(t, ctx, seed, "SELECT count(*) FROM drill.meters"); got != strconv.Itoa(documents) {
		t.Fatalf("seeded %q rows, want %d", got, documents)
	}
	res, err := seed.Exec(ctx, sandbox.ExecRequest{
		// taosdump writes dump_result.txt into the directory it runs in,
		// not into the one -o names (measured), so the seed runs where the
		// artifact is written — the way an operator's backup job would, and
		// the reason the dump can date itself at all.
		Argv: []string{"bash", "-c", "set -e; export TAOS_FQDN=localhost TAOS_FIRST_EP=localhost:6030; " +
			"rm -rf /tmp/dump; mkdir -p /tmp/dump; cd /tmp/dump && taosdump -D " + database + " -o /tmp/dump"},
	})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("taosdump: %v exit=%d %s", err, res.ExitCode, res.Stderr)
	}
	dest := filepath.Join(t.TempDir(), "dump")
	if out, err := exec.CommandContext(ctx, "docker", "cp",
		seed.ID()+":/tmp/dump", dest).CombinedOutput(); err != nil {
		t.Fatalf("copy the dump out: %v: %s", err, out)
	}
	return dest
}

// innerDumpDir is the payload directory taosdump wrote inside the output
// directory — what the tool itself has to be pointed at.
func innerDumpDir(t *testing.T, dump string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dump, "taosdump.*"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("find the payload directory: %v (%d matches)", err, len(matches))
	}
	return matches[0]
}

// damageOneAvro truncates one data file, leaving the rest of the backup
// intact — what a half-written copy looks like.
func damageOneAvro(t *testing.T, dump string) {
	t.Helper()
	// The data directory is `data0` on 3.3.5.8 and `data0-<hash>` on
	// 3.3.6.13 (measured), so the glob covers both rather than the one
	// the development machine happened to produce.
	matches, err := filepath.Glob(filepath.Join(dump, "taosdump.*", "data0*", "*.avro"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("find an avro file: %v (%d matches)", err, len(matches))
	}
	if err := os.Truncate(matches[0], 300); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

// runCheck runs one statement through the runner the adapter publishes,
// so the suite exercises the script a drill's checks actually use.
func runCheck(t *testing.T, ctx context.Context, sbx *docker.Sandbox, statement string) string {
	t.Helper()
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{
		Argv: []string{"bash", "-c", runnerScriptForTest(t), "bash", statement},
	})
	if err != nil {
		t.Fatalf("check %q: %v", statement, err)
	}
	if out.ExitCode != 0 {
		return "error: " + strings.TrimSpace(string(out.Stderr))
	}
	return strings.TrimSpace(string(out.Stdout))
}

// runnerScriptForTest reads the runner out of the probe response, so the
// suite exercises the script the adapter actually publishes.
func runnerScriptForTest(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash("testdata/probe_response.golden"))
	if err != nil {
		t.Fatalf("read probe golden: %v", err)
	}
	probe := struct {
		Payload struct {
			SQLRunner struct {
				Argv []string `json:"argv"`
			} `json:"sql_runner"`
		} `json:"payload"`
	}{}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("parse probe golden: %v", err)
	}
	if len(probe.Payload.SQLRunner.Argv) < 3 {
		t.Fatal("probe response declares no sql_runner script")
	}
	return probe.Payload.SQLRunner.Argv[2]
}

// awaitServing starts the engine in an idle sandbox and waits for the
// endpoint the checks use — what the adapter does, done here for the seed
// server the suite fills by hand.
func awaitServing(t *testing.T, ctx context.Context, sbx *docker.Sandbox) {
	t.Helper()
	// The same start the adapter performs, for the same reasons: the
	// image's entrypoint is not dependable here, and the engine has to be
	// pinned to loopback because an image can carry a name that resolves
	// to nothing in a zero-ingress sandbox (scripts.go).
	if _, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{"bash", "-c",
		`export TAOS_FQDN=localhost TAOS_FIRST_EP=localhost:6030
		 setsid taosd </dev/null >/tmp/seed-taosd.log 2>&1 &
		 for i in $(seq 1 40); do taos -s "show databases;" >/dev/null 2>&1 && break; sleep 0.5; done
		 setsid taosadapter </dev/null >/tmp/seed-taosadapter.log 2>&1 &
		 echo started`}}); err != nil {
		t.Fatalf("start the seed engine: %v", err)
	}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		// The same signal the adapter waits for: a node the cluster calls
		// ready, not merely a query that answers (scripts.go).
		out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{"bash", "-c",
			`curl -sf -u root:taosdata -d "SELECT count(*) FROM information_schema.ins_dnodes WHERE status = 'ready'" ` +
				`http://127.0.0.1:6041/rest/sql | grep -q '"data":\[\[[1-9]'`}})
		if err == nil && out.ExitCode == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the server in the sandbox never answered: %s", sandboxDiagnosis(t, ctx, sbx))
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// sandboxDiagnosis is what a server that never answered left behind. A
// suite that reports only its own timeout says nothing a maintainer can
// act on, and the container is gone by the time anyone reads the log.
func sandboxDiagnosis(t *testing.T, ctx context.Context, sbx *docker.Sandbox) string {
	t.Helper()
	var parts []string
	if out, err := exec.CommandContext(ctx, "docker", "logs", "--tail", "25", sbx.ID()).CombinedOutput(); err == nil {
		parts = append(parts, "container output: "+strings.TrimSpace(string(out)))
	}
	res, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{"bash", "-c",
		// Which of the two processes is missing answers most of it: taosd
		// serves the native client, taosadapter the endpoint the checks
		// use, and they fail independently.
		`ps -eo comm | sort -u | grep -i taos | tr '\n' ' '; echo; ` +
			`taos -s "show databases;" >/dev/null 2>&1 && echo "native client: answers" || echo "native client: silent"; ` +
			`tail -n 5 /var/log/taos/taosdlog.0 2>/dev/null; ` +
			`grep -iE "error|fail|cannot" /var/log/taos/taosadapter_*.log 2>/dev/null | tail -n 5`}})
	if err == nil {
		parts = append(parts, "inside: "+strings.TrimSpace(string(res.Stdout)))
	}
	if len(parts) == 0 {
		return "the sandbox answered nothing at all — its container is gone, which is a failure of the " +
			"sandbox rather than of the engine"
	}
	return strings.Join(parts, " || ")
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

func buildAdapterOnPath(t *testing.T, ctx context.Context) {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", filepath.Join(dir, "probavi-adapter-tdengine"), ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, out)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
