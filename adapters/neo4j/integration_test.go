//go:build integration

package main_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

// sandboxParams returns the documented drill-config sandbox params. The
// command override is not a convenience: a dump can only be loaded into a
// database no server has mounted, and the initial password only takes
// effect before the first start, so the image must idle and the adapter
// starts the engine itself. The memory limit is the documented floor —
// measured: 512 MiB never becomes ready, 768 MiB serves with about
// 420 MiB in use.
func sandboxParams(t *testing.T) map[string]string {
	return map[string]string{
		"image":   verifiedImage(t),
		"command": "sleep infinity",
		"memory":  "2g",
	}
}

const (
	orders    = 500
	customers = 100
)

// TestEndToEndRestoreDrill proves the nineteenth engine through the
// unchanged core: the docker provider, the core-side protocol client and
// this adapter — as separate processes — restore a genuine
// `neo4j-admin database dump` and validate it through the
// probe-declared sql_runner's cypher-shell bridge.
func TestEndToEndRestoreDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	provider := docker.New(nil)

	// The fixture's file name is deliberately not <database>.dump: the
	// engine derives the file name it reads from the database name, and
	// an operator's backup job must not have to know that.
	fixture := filepath.Join(t.TempDir(), "orders-backup-2026-08-26.bin")
	makeFixture(t, ctx, provider, fixture)

	sbx := freshSandbox(t, ctx, provider)
	runner, err := adapter.New("neo4j", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}

	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.Name != "neo4j" || len(probe.SQLRunner.Argv) == 0 {
		t.Fatalf("probe = %+v", probe)
	}

	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "neo4j_dump", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if res.Timings.RestoreSeconds <= 0 || res.Timings.EngineReadySeconds <= 0 || res.Timings.TransferSeconds <= 0 {
		t.Errorf("timings = %+v, want real measurements", res.Timings)
	}
	if !strings.HasPrefix(res.SourceIdentity.Checksum, "sha256:") || res.SourceIdentity.SizeBytes == 0 {
		t.Errorf("source identity = %+v", res.SourceIdentity)
	}
	if res.Connection.Database != "neo4j" || res.Connection.User != "neo4j" {
		t.Errorf("connection = %+v", res.Connection)
	}

	health, err := runner.Healthcheck(ctx, &res.Connection, res.State, sbx)
	if err != nil {
		t.Fatalf("healthcheck: %v", err)
	}
	if !health.Healthy {
		t.Fatalf("healthcheck = %+v, want healthy", health)
	}

	// Validate the restored graph through the probe-declared sql_runner —
	// exactly how internal/checks runs checks without engine knowledge.
	// The check text is Cypher: that is this adapter's documented check
	// dialect.
	checks := map[string]struct{ cypher, want string }{
		"node count": {
			"MATCH (n) RETURN count(n)", fmt.Sprint(orders + customers)},
		"relationships survived": {
			"MATCH ()-[r:PLACED]->() RETURN count(r)", fmt.Sprint(customers)},
		"property values survived": {
			"MATCH (o:Order {id: 1}) RETURN o.sku", "SKU-0001"},
		"a constraint came back with the data": {
			"SHOW CONSTRAINTS YIELD name RETURN count(name)", "1"},
		"multi-column rows are tab separated": {
			"MATCH (o:Order {id: 7}) RETURN o.id, o.sku", "7\tSKU-0007"},
		"a value holding the column separator stays one column": {
			"RETURN 'Budapest, Hungary' AS place", "Budapest, Hungary"},
	}
	for name, tt := range checks {
		t.Run(name, func(t *testing.T) {
			out, exit := runCheck(t, ctx, sbx, probe, &res.Connection, tt.cypher)
			if exit != 0 {
				t.Fatalf("check exited %d: %s", exit, out)
			}
			if out != tt.want {
				t.Errorf("check returned %q, want %q — the restore did not carry the data", out, tt.want)
			}
		})
	}

	// A check reads what the drill restored; it may not change it.
	t.Run("a check that writes is refused", func(t *testing.T) {
		if _, exit := runCheck(t, ctx, sbx, probe, &res.Connection, "CREATE (:Sneaky)"); exit == 0 {
			t.Error("a writing check succeeded — a check must not alter the artifact its evidence is about")
		}
	})

	teardown, err := runner.Teardown(ctx, res.State, "completed", sbx)
	if err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if !teardown.Released {
		t.Errorf("teardown = %+v", teardown)
	}
}

// TestBrokenDumpVerdicts prove a broken backup yields the right verdict
// through the whole stack, not a generic failure — and that the two
// shapes an operator actually meets (a file that was never a dump, and
// one a transfer cut short) both arrive as source_corrupt.
func TestBrokenDumpVerdicts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	provider := docker.New(nil)

	good := filepath.Join(t.TempDir(), "good.dump")
	makeFixture(t, ctx, provider, good)
	whole, err := os.ReadFile(good)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	truncated := filepath.Join(t.TempDir(), "truncated.dump")
	if err := os.WriteFile(truncated, whole[:len(whole)/3], 0o600); err != nil {
		t.Fatalf("write truncated fixture: %v", err)
	}
	notADump := filepath.Join(t.TempDir(), "not-a-dump.dump")
	if err := os.WriteFile(notADump, []byte(strings.Repeat("this was never a Neo4j dump\n", 100)), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	for name, path := range map[string]string{"never a dump": notADump, "cut short": truncated} {
		t.Run(name, func(t *testing.T) {
			sbx := freshSandbox(t, ctx, provider)
			runner, err := adapter.New("neo4j", nil, nil)
			if err != nil {
				t.Fatalf("resolve adapter: %v", err)
			}
			_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
				Source:  adapter.ProvisionSource{Kind: "neo4j_dump", Path: path},
				Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
			}, sbx)
			var aerr *adapter.Error
			if err == nil || !errors.As(err, &aerr) || aerr.Code != "source_corrupt" {
				t.Fatalf("provision error = %v, want source_corrupt", err)
			}
		})
	}
}

// TestADumpTheServerCannotMountFailsTheDrill is the measured trap, end to
// end. Community Edition mounts only the database its configuration
// names; a dump loaded under any other name lands on disk and is never
// mounted. Everything else about the drill succeeds — the load reports
// success, the server starts, the connection works — so without the
// adapter's gate the drill would record a green that proves nothing.
func TestADumpTheServerCannotMountFailsTheDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	provider := docker.New(nil)
	fixture := filepath.Join(t.TempDir(), "orders.dump")
	makeFixture(t, ctx, provider, fixture)

	sbx := freshSandbox(t, ctx, provider)
	runner, err := adapter.New("neo4j", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "neo4j_dump", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
		Options: map[string]string{"database": "orders"},
	}, sbx)
	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) || aerr.Code != "restore_failed" {
		t.Fatalf("provision error = %v, want restore_failed", err)
	}
	// The refusal has to say what the engine does serve, or an operator
	// cannot tell this apart from a broken backup.
	for _, want := range []string{"orders", "neo4j", "system"} {
		if !strings.Contains(aerr.Message, want) {
			t.Errorf("message = %q, want it to name %q", aerr.Message, want)
		}
	}
	// And the trap itself: the load did happen, so the files are there.
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{"ls", "/data/databases"}})
	if err != nil || out.ExitCode != 0 {
		t.Fatalf("list databases: %v (exit %d)", err, out.ExitCode)
	}
	if !strings.Contains(string(out.Stdout), "orders") {
		t.Skip("the engine no longer writes an unmountable store to disk; the gate is now belt and braces")
	}
}

// TestNothingInTheSandboxExpiresTheArtifact guards the property the
// data-lifecycle survey asks of every engine: a drill sandbox must not
// apply the engine's own retention to the backup it is proving.
//
// Neo4j Community is the rare engine with nothing to suspend — measured
// across its settings, there is no TTL, no expiry and no scheduled
// deletion — so the adapter suspends nothing and this test is what keeps
// that answer honest: it fails the day an image ships a plugin or a
// default that could take rows away mid-drill.
func TestNothingInTheSandboxExpiresTheArtifact(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	provider := docker.New(nil)
	fixture := filepath.Join(t.TempDir(), "orders.dump")
	makeFixture(t, ctx, provider, fixture)

	sbx := freshSandbox(t, ctx, provider)
	runner, err := adapter.New("neo4j", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "neo4j_dump", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	// Every setting whose name suggests something expires. The two below
	// are the measured answer on the verified image: a routing-table
	// lifetime a client reads, and transaction-log pruning, which removes
	// logs rather than data. Anything else here needs a human to decide
	// whether a drill has to suspend it (adapter-development skill:
	// suspend, hold open, fence, or guard).
	known := []string{"dbms.routing_ttl", "db.tx_log.rotation.retention_policy"}
	out, exit := runCheck(t, ctx, sbx, probe, &res.Connection,
		"SHOW SETTINGS YIELD name WHERE name CONTAINS 'ttl' OR name CONTAINS 'expir' "+
			"OR name CONTAINS 'retention' OR name CONTAINS 'evict' RETURN name")
	if exit != 0 {
		t.Fatalf("settings query exited %d: %s", exit, out)
	}
	for _, name := range strings.Split(out, "\n") {
		if name = strings.TrimSpace(name); name == "" {
			continue
		}
		if !slices.Contains(known, name) {
			t.Errorf("the sandbox engine has a setting this adapter has never considered: %s — "+
				"decide whether a drill must suspend it before the next release", name)
		}
	}

	// A check that runs long must not be killed halfway: a transaction
	// timeout would turn a big restored graph's validation into a failure
	// that says nothing about the backup.
	if out, exit := runCheck(t, ctx, sbx, probe, &res.Connection,
		"SHOW SETTINGS YIELD name, value WHERE name = 'db.transaction.timeout' RETURN value"); exit != 0 || out != "0s" {
		t.Errorf("db.transaction.timeout = %q (exit %d), want 0s — a check must not be cut short", out, exit)
	}

	// And nothing may have been loaded that runs jobs of its own: APOC's
	// TTL removes nodes on a schedule, and a zero-ingress sandbox cannot
	// download it — but an image could ship it.
	plugins, err := sbx.Exec(ctx, sandbox.ExecRequest{
		Argv: []string{"bash", "-c", `ls /var/lib/neo4j/plugins/*.jar 2>/dev/null | wc -l`}})
	if err != nil || plugins.ExitCode != 0 {
		t.Fatalf("list plugins: %v (exit %d)", err, plugins.ExitCode)
	}
	if got := strings.TrimSpace(string(plugins.Stdout)); got != "0" {
		t.Errorf("the sandbox image ships %s plugin jars — a plugin can run jobs that delete data mid-drill", got)
	}
}

// runCheck renders the probe-declared sql_runner exactly as
// internal/checks does and runs one check in the sandbox.
func runCheck(t *testing.T, ctx context.Context, sbx *docker.Sandbox, probe *adapter.ProbeResult,
	conn *adapter.Connection, cypher string) (string, int) {
	t.Helper()
	argv := make([]string, 0, len(probe.SQLRunner.Argv))
	for _, a := range probe.SQLRunner.Argv {
		a = strings.ReplaceAll(a, "{{user}}", conn.User)
		a = strings.ReplaceAll(a, "{{database}}", conn.Database)
		a = strings.ReplaceAll(a, "{{sql}}", cypher)
		argv = append(argv, a)
	}
	env := make(map[string]string, len(probe.SQLRunner.Env))
	for k, v := range probe.SQLRunner.Env {
		v = strings.ReplaceAll(v, "{{user}}", conn.User)
		env[k] = strings.ReplaceAll(v, "{{database}}", conn.Database)
	}
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: argv, Env: env})
	if err != nil {
		t.Fatalf("sql_runner exec: %v", err)
	}
	if out.ExitCode != 0 {
		return strings.TrimSpace(string(out.Stderr)), out.ExitCode
	}
	return strings.TrimSpace(string(out.Stdout)), 0
}

// buildAdapterOnPath builds the adapter binary and puts it on PATH under
// its protocol name.
func buildAdapterOnPath(t *testing.T, ctx context.Context) {
	t.Helper()
	binDir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o",
		filepath.Join(binDir, "probavi-adapter-neo4j"), ".").CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// makeFixture seeds a graph in a sandbox of its own and extracts a real
// `neo4j-admin database dump` to the host — the artifact an operator's
// backup job would produce.
func makeFixture(t *testing.T, ctx context.Context, provider *docker.Provider, dest string) {
	t.Helper()
	seed := freshSandbox(t, ctx, provider)
	seedPassword := "Probavi-Fixture-Seed-0"

	mustExec(t, ctx, seed, "bash", "-c",
		`set -u
name=$(hostname); getent hosts "$name" >/dev/null 2>&1 || printf '127.0.0.1 %s\n' "$name" >> /etc/hosts
neo4j-admin dbms set-initial-password "$1" >/dev/null
neo4j start >/dev/null`, "bash", seedPassword)
	awaitReady(t, ctx, seed, seedPassword)

	seedCypher := fmt.Sprintf(`CREATE CONSTRAINT order_id IF NOT EXISTS FOR (o:Order) REQUIRE o.id IS UNIQUE;
UNWIND range(1, %d) AS i CREATE (:Order {id: i, sku: 'SKU-' + right('000' + toString(i), 4), total: i * 1.5});
MATCH (o:Order) WHERE o.id <= %d CREATE (:Customer {id: o.id})-[:PLACED]->(o);`, orders, customers)
	mustCypher(t, ctx, seed, seedPassword, seedCypher)

	// A dump can only be taken from a database no server has mounted.
	mustExec(t, ctx, seed, "bash", "-c",
		`set -u
neo4j stop >/dev/null
neo4j-admin database dump neo4j --to-path=/tmp >/dev/null`, "bash")
	copyOut(t, ctx, seed, "/tmp/neo4j.dump", dest)
}

// awaitReady waits for a seed engine to answer queries.
func awaitReady(t *testing.T, ctx context.Context, sbx *docker.Sandbox, password string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		out, err := sbx.Exec(ctx, sandbox.ExecRequest{
			Argv: []string{"cypher-shell", "--format", "plain", "--non-interactive", "RETURN 1"},
			Env:  map[string]string{"NEO4J_USERNAME": "neo4j", "NEO4J_PASSWORD": password},
		})
		if err == nil && out.ExitCode == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("seed engine never became ready: %v", err)
		}
		select {
		case <-ctx.Done():
			t.Fatal("cancelled while waiting for the seed engine")
		case <-time.After(time.Second):
		}
	}
}

// mustCypher runs seed statements, one per line, through cypher-shell.
func mustCypher(t *testing.T, ctx context.Context, sbx *docker.Sandbox, password, cypher string) {
	t.Helper()
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{
		Argv:  []string{"cypher-shell", "--format", "plain", "--non-interactive", "--fail-fast"},
		Env:   map[string]string{"NEO4J_USERNAME": "neo4j", "NEO4J_PASSWORD": password},
		Stdin: []byte(cypher + "\n"),
	})
	if err != nil || out.ExitCode != 0 {
		t.Fatalf("seed cypher: %v (exit %d, stderr %s)", err, out.ExitCode, out.Stderr)
	}
}

func mustExec(t *testing.T, ctx context.Context, sbx *docker.Sandbox, argv ...string) {
	t.Helper()
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: argv})
	if err != nil || out.ExitCode != 0 {
		t.Fatalf("exec %v: %v (exit %d, stderr %s)", argv, err, out.ExitCode, out.Stderr)
	}
}

func copyOut(t *testing.T, ctx context.Context, sbx *docker.Sandbox, containerPath, dest string) {
	t.Helper()
	if out, err := exec.CommandContext(ctx, "docker", "cp", sbx.ID()+":"+containerPath, dest).CombinedOutput(); err != nil {
		t.Fatalf("extract fixture: %v: %s", err, out)
	}
}

func freshSandbox(t *testing.T, ctx context.Context, provider *docker.Provider) *docker.Sandbox {
	t.Helper()
	sbx, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	t.Cleanup(func() { destroy(t, sbx) })
	return sbx
}

func destroy(t *testing.T, sbx *docker.Sandbox) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sbx.Destroy(ctx); err != nil {
		t.Errorf("destroy sandbox: %v", err)
	}
}
