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
// the manifest already lists. The manifest and this suite read the same
// values, so docs/capabilities.json can never claim an engine version CI
// does not actually restore from.
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

// restoreParams are the documented drill-config sandbox params: idle, so
// the adapter can replace the data root and start the engine itself.
func restoreParams(t *testing.T) map[string]string {
	return map[string]string{
		"image":   verifiedImage(t),
		"command": "sleep infinity",
		"memory":  "1g",
	}
}

// serverParams start the image the way it ships — the engine running — for
// seeding a backup and for the busy-sandbox refusal.
func serverParams(t *testing.T) map[string]string {
	return map[string]string{
		"image":                     verifiedImage(t),
		"memory":                    "1g",
		"env.QDB_TELEMETRY_ENABLED": "false",
	}
}

const documents = 250

// TestEndToEndRestoreDrill proves the engine through the adapter the core
// actually runs: a backup taken from one server is restored into a sandbox
// that has never seen it, and the checks read what the backup held.
func TestEndToEndRestoreDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	provider := docker.New(nil)
	backup := makeBackup(t, ctx, provider, true)

	runner, err := adapter.New("questdb", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	sbx := freshSandbox(t, ctx, provider, restoreParams(t))
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "questdb_checkpoint", Path: backup},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if res.SourceIdentity.CreatedAt != nil {
		t.Errorf("created_at = %v, want null — nothing in the artifact dates it", *res.SourceIdentity.CreatedAt)
	}
	if res.Timings.RestoreSeconds <= 0 || res.Timings.EngineReadySeconds <= 0 {
		t.Errorf("timings = %+v, want measurements of both halves of the recovery", res.Timings)
	}

	for _, tc := range []struct{ name, query, want string }{
		{"every row came back", "SELECT count(*) FROM orders", strconv.Itoa(documents)},
		// The count above is served from transaction metadata; this one
		// has to read the column, which is what a truncation would show.
		{"the column data is real", "SELECT sum(id) FROM orders", strconv.Itoa(documents * (documents + 1) / 2)},
		{"a filter answers", "SELECT count(*) FROM orders WHERE id <= 10", "10"},
		{"the non-WAL table came back too", "SELECT count(*) FROM legacy", "120"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := runCheck(t, ctx, sbx, tc.query); got != tc.want {
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

	t.Run("teardown is idempotent", func(t *testing.T) {
		for i := range 2 {
			if _, err := runner.Teardown(ctx, res.State, "completed", sbx); err != nil {
				t.Fatalf("teardown %d: %v", i, err)
			}
		}
	})
}

// TestUncheckpointedCopyDrillsOnlyAsData pins the fence from both sides:
// the claim is refused, the artifact still drills under the kind that
// makes no claim.
func TestUncheckpointedCopyDrillsOnlyAsData(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	provider := docker.New(nil)
	backup := makeBackup(t, ctx, provider, false)

	runner, err := adapter.New("questdb", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}

	sbx := freshSandbox(t, ctx, provider, restoreParams(t))
	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "questdb_checkpoint", Path: backup},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) || aerr.Code != "invalid_request" {
		t.Fatalf("provision error = %v, want invalid_request for a copy with no checkpoint held", err)
	}
	if !strings.Contains(aerr.Message, "questdb_data") {
		t.Errorf("message = %q, want it to name the kind that fits the artifact", aerr.Message)
	}

	second := freshSandbox(t, ctx, provider, restoreParams(t))
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "questdb_data", Path: backup},
		Sandbox: adapter.SandboxInfo{ScratchDir: second.ScratchDir()},
	}, second)
	if err != nil {
		t.Fatalf("provision as questdb_data: %v", err)
	}
	if got := runCheck(t, ctx, second, "SELECT count(*) FROM orders"); got != strconv.Itoa(documents) {
		t.Errorf("rows = %q, want %d", got, documents)
	}
	_ = res
}

// TestBusySandboxIsRefused pins the idle requirement against the real
// image: the drill that forgot the command override is told what to add,
// rather than having its restore fight a running server for the files.
func TestBusySandboxIsRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	provider := docker.New(nil)
	backup := makeBackup(t, ctx, provider, true)

	runner, err := adapter.New("questdb", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	busy := freshSandbox(t, ctx, provider, serverParams(t))
	awaitServing(t, ctx, busy)
	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "questdb_checkpoint", Path: backup},
		Sandbox: adapter.SandboxInfo{ScratchDir: busy.ScratchDir()},
	}, busy)
	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) || aerr.Code != "invalid_request" {
		t.Fatalf("provision error = %v, want invalid_request against a serving sandbox", err)
	}
	if !strings.Contains(aerr.Message, "sleep infinity") {
		t.Errorf("message = %q, want the parameter that fixes the drill", aerr.Message)
	}
}

// TestRowCountAloneDoesNotProveTheData pins the limitation the README
// states, against the engine rather than against a belief: a column
// truncated to a fraction of itself leaves count(*) answering in full,
// with no error anywhere, and only a check that reads the column notices.
func TestRowCountAloneDoesNotProveTheData(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	provider := docker.New(nil)
	backup := makeBackup(t, ctx, provider, true)
	truncateColumn(t, backup)

	runner, err := adapter.New("questdb", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	sbx := freshSandbox(t, ctx, provider, restoreParams(t))
	if _, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "questdb_checkpoint", Path: backup},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx); err != nil {
		t.Fatalf("provision of a damaged artifact: %v — the engine accepts it, which is the point", err)
	}
	if got := runCheck(t, ctx, sbx, "SELECT count(*) FROM orders"); got != strconv.Itoa(documents) {
		t.Errorf("count(*) = %q, want %d — the engine serves the metadata's claim", got, documents)
	}
	whole := strconv.Itoa(documents * (documents + 1) / 2)
	if got := runCheck(t, ctx, sbx, "SELECT sum(id) FROM orders"); got == whole {
		t.Errorf("sum(id) = %q, want a short sum — the truncation was not applied", got)
	}
}

// TestTTLDataSurvivesAReadOnlyDrill guards the data-lifecycle property.
// QuestDB drops expired partitions when a table is written, and a drill
// only reads: a backup whose table declares a one-hour TTL restores whole
// and stays whole, however old the backup is. Nothing is suspended to
// achieve that, so this test is what keeps it true.
func TestTTLDataSurvivesAReadOnlyDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	provider := docker.New(nil)
	backup := makeTTLBackup(t, ctx, provider)

	runner, err := adapter.New("questdb", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	sbx := freshSandbox(t, ctx, provider, restoreParams(t))
	if _, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "questdb_checkpoint", Path: backup},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx); err != nil {
		t.Fatalf("provision: %v", err)
	}
	first := runCheck(t, ctx, sbx, "SELECT count(*) FROM aged")
	if first == "0" || first == "" {
		t.Fatalf("restored rows = %q, want the backup's rows despite the TTL", first)
	}
	time.Sleep(5 * time.Second)
	if second := runCheck(t, ctx, sbx, "SELECT count(*) FROM aged"); second != first {
		t.Errorf("rows went from %s to %s while the drill only read: something is enforcing the TTL",
			first, second)
	}
	if ttl := runCheck(t, ctx, sbx, "SELECT ttlValue FROM tables() WHERE table_name = 'aged'"); ttl != "1" {
		t.Errorf("ttlValue = %q, want the operator's own declaration untouched", ttl)
	}
}

// makeBackup seeds a server and copies its data root out, with or without
// a checkpoint held while the copy runs.
func makeBackup(t *testing.T, ctx context.Context, provider *docker.Provider, checkpointed bool) string {
	t.Helper()
	seed := freshSandbox(t, ctx, provider, serverParams(t))
	awaitServing(t, ctx, seed)
	sql(t, ctx, seed, "CREATE TABLE orders (id LONG, n INT, t STRING, ts TIMESTAMP) TIMESTAMP(ts) PARTITION BY DAY WAL")
	sql(t, ctx, seed, fmt.Sprintf(
		"INSERT INTO orders SELECT x, x, 'lorem ' || x, dateadd('m', cast(x AS INT), "+
			"to_timestamp('2026-09-01T00:00:00', 'yyyy-MM-ddTHH:mm:ss')) FROM long_sequence(%d)", documents))
	sql(t, ctx, seed, "CREATE TABLE legacy (id LONG, ts TIMESTAMP) TIMESTAMP(ts) PARTITION BY DAY BYPASS WAL")
	sql(t, ctx, seed, "INSERT INTO legacy SELECT x, dateadd('m', cast(x AS INT), "+
		"to_timestamp('2026-09-01T00:00:00', 'yyyy-MM-ddTHH:mm:ss')) FROM long_sequence(120)")
	awaitRows(t, ctx, seed, "orders", documents)
	if checkpointed {
		sql(t, ctx, seed, "CHECKPOINT CREATE")
	}
	dest := copyOut(t, ctx, seed, "/var/lib/questdb", filepath.Join(t.TempDir(), "backup"))
	if checkpointed {
		sql(t, ctx, seed, "CHECKPOINT RELEASE")
	}
	return dest
}

// makeTTLBackup seeds a table that declares a TTL and holds rows older
// than it, which is what every backup of such a table becomes the moment
// it is older than the TTL.
func makeTTLBackup(t *testing.T, ctx context.Context, provider *docker.Provider) string {
	t.Helper()
	params := serverParams(t)
	// Seeded with wall-clock enforcement off so the rows can be written at
	// their own instant; the restore runs with the engine's default.
	params["env.QDB_CAIRO_TTL_USE_WALL_CLOCK"] = "false"
	seed := freshSandbox(t, ctx, provider, params)
	awaitServing(t, ctx, seed)
	sql(t, ctx, seed, "CREATE TABLE aged (id LONG, ts TIMESTAMP) TIMESTAMP(ts) PARTITION BY HOUR TTL 1 HOUR WAL")
	sql(t, ctx, seed, "INSERT INTO aged SELECT x, dateadd('m', cast(x AS INT), "+
		"to_timestamp('2026-09-01T10:00:00', 'yyyy-MM-ddTHH:mm:ss')) FROM long_sequence(120)")
	awaitRows(t, ctx, seed, "aged", 61)
	sql(t, ctx, seed, "CHECKPOINT CREATE")
	dest := copyOut(t, ctx, seed, "/var/lib/questdb", filepath.Join(t.TempDir(), "ttl-backup"))
	sql(t, ctx, seed, "CHECKPOINT RELEASE")
	return dest
}

// truncateColumn cuts a column file down to a few values, leaving the
// transaction metadata claiming the whole table.
func truncateColumn(t *testing.T, backup string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(backup, "db", "orders~*", "2026-09-01*", "id.d"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("find a column file to damage: %v (%d matches)", err, len(matches))
	}
	if err := os.Truncate(matches[0], 64); err != nil {
		t.Fatalf("truncate column: %v", err)
	}
}

// sql runs one statement against a running sandbox server.
func sql(t *testing.T, ctx context.Context, sbx *docker.Sandbox, statement string) string {
	t.Helper()
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{
		Argv: []string{"bash", "-c",
			`set -u; curl -s -w '\n%{http_code}' -G "http://127.0.0.1:9000/exec" --data-urlencode "query=$1"`,
			"bash", statement},
	})
	if err != nil {
		t.Fatalf("statement %q: %v", statement, err)
	}
	body := strings.TrimSpace(string(out.Stdout))
	if !strings.HasSuffix(body, "200") {
		t.Fatalf("statement %q: %s", statement, body)
	}
	return body
}

// awaitServing waits for a sandbox that starts the engine itself.
func awaitServing(t *testing.T, ctx context.Context, sbx *docker.Sandbox) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		out, err := sbx.Exec(ctx, sandbox.ExecRequest{
			Argv: []string{"bash", "-c", `curl -sf -o /dev/null "http://127.0.0.1:9000/exec?query=select%201"`},
		})
		if err == nil && out.ExitCode == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the seed server never started serving")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// awaitRows waits for a WAL table to have applied its inserts.
func awaitRows(t *testing.T, ctx context.Context, sbx *docker.Sandbox, table string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		got := runCheck(t, ctx, sbx, "SELECT count(*) FROM "+table)
		if got == strconv.Itoa(want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s holds %s rows, want %d — the WAL never applied", table, got, want)
		}
		time.Sleep(500 * time.Millisecond)
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
		t.Fatalf("check %q: exit %d: %s", statement, out.ExitCode, out.Stderr)
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

func freshSandbox(t *testing.T, ctx context.Context, provider *docker.Provider, params map[string]string) *docker.Sandbox {
	t.Helper()
	sbx, err := provider.Create(ctx, params)
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

func buildAdapterOnPath(t *testing.T, ctx context.Context) {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", filepath.Join(dir, "probavi-adapter-questdb"), ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, out)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
