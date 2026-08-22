//go:build integration

package main_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/probavi/probavi/internal/adapter"
	"github.com/probavi/probavi/internal/capabilities"
	"github.com/probavi/probavi/internal/checks"
	"github.com/probavi/probavi/internal/config"
	"github.com/probavi/probavi/internal/sandbox"
	"github.com/probavi/probavi/internal/sandbox/docker"
)

// verifiedImage is the official image this run's instance comes from:
// the manifest's baseline, or the version-matrix job's PROBAVI_IT_IMAGE
// when it names one the manifest already lists (docs/engine-versions.md
// §2). No wrapper is needed: the image has no entrypoint, so command:
// sleep infinity idles it (measured).
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

// internalNetwork creates the network the README tells operators to
// create: internal, so the sandbox has the interface the instance
// insists on and no route anywhere. Removed when the test ends.
func internalNetwork(t *testing.T, ctx context.Context) string {
	t.Helper()
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	name := "probavi-it-" + hex.EncodeToString(suffix)
	if out, err := exec.CommandContext(ctx, "docker", "network", "create", "--internal", name).CombinedOutput(); err != nil {
		t.Fatalf("create internal network: %v: %s", err, out)
	}
	t.Cleanup(func() {
		if out, err := exec.Command("docker", "network", "rm", name).CombinedOutput(); err != nil {
			t.Errorf("remove internal network: %v: %s", err, out)
		}
	})
	return name
}

// sandboxParams are the README's: the official image idle, 3g of memory
// (the measured floor — at 2 GiB the instance mounts and is killed while
// opening), and the internal network.
func sandboxParams(image, network string) map[string]string {
	return map[string]string{"image": image, "command": "sleep infinity", "memory": "3g", "network": network}
}

// productionStart starts the instance the way the image does in
// production — its own run script, listener and all, every default in
// force — and waits for it. The fixture must be what production would
// have produced, and the control half must run what production runs.
const productionStart = `export NLS_LANG=.AL32UTF8
/opt/oracle/runOracle.sh --nowait > /tmp/start.log 2>&1 || { tail -n 30 /tmp/start.log >&2; exit 1; }
`

// seedScript creates a schema with five rows — one holding a tab, one
// non-ASCII — and a scheduler job the schema's owner would plausibly
// keep: a purge of rows older than two minutes, every five seconds. Two
// rows are a minute old when the export runs, so the dump holds all
// five while every one of them is past the purge window by the time a
// drill imports it: a sandbox in which the job runs loses rows at once.
// The export is a plain schema-mode expdp over the bequeath adapter.
const seedScript = `set -e
` + productionStart + `
ORACLE_PDB_SID=FREEPDB1 sqlplus -S -L / as sysdba <<'EOF'
whenever sqlerror exit 1
create user probavi_app identified by "Probavi_pw1" quota unlimited on users;
grant create session, create table, create job to probavi_app;
create table probavi_app.orders (id number primary key, customer varchar2(40), created_at timestamp with time zone);
insert into probavi_app.orders values (1, 'ada',   systimestamp - interval '1' minute);
insert into probavi_app.orders values (2, 'grace', systimestamp - interval '1' minute);
insert into probavi_app.orders values (3, 'linus', systimestamp);
insert into probavi_app.orders values (4, 'tab' || chr(9) || 'in value', systimestamp);
insert into probavi_app.orders values (5, unistr('\00fcn\00efc\00f6d\00e9'), systimestamp);
commit;
begin
  dbms_scheduler.create_job(job_name => 'PROBAVI_APP.PURGE_OLD_ORDERS', job_type => 'PLSQL_BLOCK',
    job_action => 'begin delete from probavi_app.orders where created_at < systimestamp - interval ''2'' minute; commit; end;',
    repeat_interval => 'FREQ=SECONDLY; INTERVAL=5', enabled => true);
end;
/
EOF
mkdir -p /tmp/fixture
ORACLE_PDB_SID=FREEPDB1 sqlplus -S -L / as sysdba <<'EOF'
whenever sqlerror exit 1
create or replace directory probavi_fixture as '/tmp/fixture';
EOF
ORACLE_PDB_SID=FREEPDB1 expdp \"/ as sysdba\" schemas=PROBAVI_APP directory=probavi_fixture dumpfile=orders.dmp logfile=orders.log > /tmp/expdp.out 2>&1 || { tail -n 20 /tmp/expdp.out >&2; exit 1; }
rows=$(ORACLE_PDB_SID=FREEPDB1 sqlplus -S -L / as sysdba <<'EOF'
set heading off feedback off pagesize 0
select count(*) from probavi_app.orders;
EOF
)
[ "$(echo $rows)" = 5 ] || { echo "the source lost rows before the export finished: $rows" >&2; exit 1; }`

// controlScript is what a drill without the adapter's pins would do:
// start the instance as production does and import the dump, then watch
// the table for sixty seconds and report the first moment a row is gone.
const controlScript = `set -e
` + productionStart + `
export ORACLE_PDB_SID=FREEPDB1
sqlplus -S -L / as sysdba <<'EOF'
whenever sqlerror exit 1
create or replace directory probavi_control as '/tmp/control';
EOF
impdp \"/ as sysdba\" directory=probavi_control dumpfile=orders.dmp logfile=control.log > /tmp/impdp.out 2>&1 || { tail -n 20 /tmp/impdp.out >&2; exit 1; }
for i in $(seq 1 12); do
  rows=$(sqlplus -S -L / as sysdba <<'EOF'
set heading off feedback off pagesize 0
select count(*) from probavi_app.orders;
EOF
)
  rows=$(echo $rows)
  if [ "$rows" -lt 5 ]; then echo "rows=$rows after $((i*5-5))s"; exit 0; fi
  sleep 5
done
echo "rows=$rows after 60s: nothing purged"; exit 3`

// countScript reads the restored table and the job the dump carried.
const countScript = `export ORACLE_PDB_SID=FREEPDB1
sqlplus -S -L / as sysdba <<'EOF'
whenever sqlerror exit 1
set heading off feedback off pagesize 0
select 'rows=' || count(*) from probavi_app.orders;
select 'job=' || enabled || ':' || run_count from dba_scheduler_jobs where owner = 'PROBAVI_APP' and job_name = 'PURGE_OLD_ORDERS';
select 'jobq=' || value from v$parameter where name = 'job_queue_processes';
EOF`

// TestLifecycleJobsDoNotRunInTheDrill is this adapter's instance of the
// data-lifecycle rule (issue #166), proven from both sides.
//
// The control half imports the fixture into an instance exactly as the
// image runs it in production and shows the loss: within a minute the
// purge job that travelled in the dump has deleted rows, while the
// import stood as a success. Without this half the drill half would
// prove nothing — a fixture that is not deadly cannot show a pin
// working.
//
// The drill half provisions through the adapter, waits longer than the
// job's interval, and shows every row surviving with the job still
// ENABLED in the dictionary and never run — suspended, not rewritten.
// Remove job_queue_processes=0 from the launch and this test goes red.
func TestLifecycleJobsDoNotRunInTheDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	network := internalNetwork(t, ctx)
	provider := docker.New(nil)

	fixture := filepath.Join(t.TempDir(), "fixture")
	seedFixture(t, ctx, provider, image, network, seedScript, map[string]string{"/tmp/fixture": fixture})
	dump := filepath.Join(fixture, "orders.dmp")

	t.Run("the fixture loses rows without the pin", func(t *testing.T) {
		control, err := provider.Create(ctx, sandboxParams(image, network))
		if err != nil {
			t.Fatalf("create control sandbox: %v", err)
		}
		defer destroy(t, control)
		copyIntoSandbox(t, ctx, control, fixture, "/tmp/control")
		res, err := control.Exec(ctx, sandbox.ExecRequest{Argv: []string{"bash", "-c", controlScript}})
		if err != nil {
			t.Fatalf("control exec: %v", err)
		}
		if res.ExitCode != 0 {
			t.Fatalf("the control instance kept every row (exit %d: %s %s) — the fixture proves "+
				"nothing, and neither would the drill half", res.ExitCode, res.Stdout, res.Stderr)
		}
		t.Logf("control instance without the pin: %s", strings.TrimSpace(string(res.Stdout)))
	})

	sbx, err := provider.Create(ctx, sandboxParams(image, network))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("oracle", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	if _, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "oracle_datapump", Path: dump},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx); err != nil {
		t.Fatalf("provision: %v", err)
	}

	// Every row is past the purge window by now; the job fires every
	// five seconds wherever a coordinator runs it.
	time.Sleep(30 * time.Second)
	got := execScript(t, ctx, sbx, countScript)
	for _, want := range []string{"rows=5", "job=TRUE:0", "jobq=0"} {
		if !strings.Contains(got, want) {
			t.Errorf("restored state = %q, want %s: every row kept, the job ENABLED as the backup "+
				"declared it and never run, the coordinator off", got, want)
		}
	}
}

// TestEndToEndRestoreDrill proves the engine through the unchanged core:
// the docker provider, the core-side protocol client, and this adapter —
// as separate processes — import a genuine Data Pump dump into the
// image's pluggable database, and validate the rows through the core's
// own built-in checks: the generating kinds apply to this engine (the
// dialect takes SQL-standard quoted identifiers, the session's NLS
// formats render a timestamp the core parses, measured). It also reads
// back the zero-ingress claim: nothing listens on any TCP port.
func TestEndToEndRestoreDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	network := internalNetwork(t, ctx)
	provider := docker.New(nil)

	fixture := filepath.Join(t.TempDir(), "fixture")
	seedFixture(t, ctx, provider, image, network, seedScript, map[string]string{"/tmp/fixture": fixture})
	dump := filepath.Join(fixture, "orders.dmp")

	sbx, err := provider.Create(ctx, sandboxParams(image, network))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("oracle", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.Name != "oracle" || len(probe.SQLRunner.Argv) == 0 {
		t.Fatalf("probe = %+v", probe)
	}

	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source: adapter.ProvisionSource{Kind: "oracle_datapump", Path: dump,
			Params: map[string]string{"backup_timezone": "UTC"}},
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
	if res.Connection.Database != "FREEPDB1" {
		t.Errorf("connection.database = %q, want the image's pluggable database", res.Connection.Database)
	}
	// The header's own clock in the declared zone: the export ran in
	// the seed container, whose clock is UTC, within the last minutes.
	if res.SourceIdentity.CreatedAt == nil {
		t.Fatal("created_at = nil, want the dump header's clock in the declared zone")
	}
	created, err := time.Parse(time.RFC3339, *res.SourceIdentity.CreatedAt)
	if err != nil {
		t.Fatalf("created_at = %q does not parse: %v", *res.SourceIdentity.CreatedAt, err)
	}
	if age := time.Since(created); age < 0 || age > 15*time.Minute {
		t.Errorf("created_at = %s is %s old, want the export's instant minutes ago", created, age)
	}

	health, err := runner.Healthcheck(ctx, &res.Connection, res.State, sbx)
	if err != nil {
		t.Fatalf("healthcheck: %v", err)
	}
	if !health.Healthy {
		t.Fatalf("healthcheck = %+v, want healthy", health)
	}

	// Nothing listens: the listener was never started and the
	// dispatchers are off, so the network the instance insisted on
	// carries no endpoint (Docker's embedded DNS on the container's own
	// loopback is the only socket, measured).
	sockets := execScript(t, ctx, sbx, "ss -ltnH")
	for _, line := range strings.Split(strings.TrimSpace(sockets), "\n") {
		if line == "" || strings.Contains(line, "127.0.0.11:") {
			continue
		}
		t.Errorf("a TCP socket listens in the drill sandbox: %s", line)
	}

	// The checks are the core's own — generated SQL, the freshness age
	// computed in Go against an injected clock — plus a custom check
	// with its expect. Identifiers are named as the dictionary stores
	// them (upper case), because the core quotes them.
	five := int64(5)
	now := time.Now
	deps := checks.Deps{
		Exec:   sbx,
		Runner: checks.Runner{Argv: probe.SQLRunner.Argv, Env: probe.SQLRunner.Env},
		Target: checks.Target{User: res.Connection.User, Database: res.Connection.Database},
		Now:    now,
	}
	results, err := checks.Run(ctx, []config.Check{
		{Builtin: config.CheckTableExists, Table: "PROBAVI_APP.ORDERS"},
		{Builtin: config.CheckRowCount, Table: "PROBAVI_APP.ORDERS", Min: &five, Max: &five},
		{Builtin: config.CheckFreshness, Table: "PROBAVI_APP.ORDERS", Column: "CREATED_AT", MaxAge: config.Duration(time.Hour)},
		{Name: "unicode-survives", SQL: "select customer from probavi_app.orders where id = 5", Expect: config.ScalarFromString("ünïcödé")},
		{Name: "tab-in-value", SQL: "select customer from probavi_app.orders where id = 4", Expect: config.ScalarFromString("tab\tin value")},
	}, deps)
	if err != nil {
		t.Fatalf("checks: %v", err)
	}
	for _, r := range results {
		if !r.OK {
			t.Errorf("check %s failed: %s", r.Name, r.Detail)
		}
	}
	if len(results) != 5 {
		t.Errorf("%d results, want 5", len(results))
	}
}

// TestDamagedDumpsAreRefused covers the measured failure shapes: a file
// truncated mid-block is refused by the engine's header reader within
// seconds, and a dump damaged in its middle — whose header is intact —
// kills the Data Pump worker and would hang the client forever, which
// the watchdog turns into a verdict.
func TestDamagedDumpsAreRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	network := internalNetwork(t, ctx)
	provider := docker.New(nil)

	fixture := filepath.Join(t.TempDir(), "fixture")
	seedFixture(t, ctx, provider, image, network, seedScript, map[string]string{"/tmp/fixture": fixture})
	good, err := os.ReadFile(filepath.Join(fixture, "orders.dmp"))
	if err != nil {
		t.Fatal(err)
	}

	damaged := append([]byte(nil), good...)
	for i := len(damaged) / 2; i < len(damaged)/2+4096 && i < len(damaged); i++ {
		damaged[i] = 0xff
	}
	tests := []struct {
		name     string
		bytes    []byte
		wantCode string
		wantMsg  string
	}{
		{"truncated", good[:len(good)-len(good)%4096-100], "source_corrupt", "ORA-39211"},
		{"damaged mid-file", damaged, "source_corrupt", "never returned"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "orders.dmp")
			if err := os.WriteFile(path, tt.bytes, 0o600); err != nil {
				t.Fatal(err)
			}
			sbx, err := provider.Create(ctx, sandboxParams(image, network))
			if err != nil {
				t.Fatalf("create sandbox: %v", err)
			}
			defer destroy(t, sbx)
			runner, err := adapter.New("oracle", nil, nil)
			if err != nil {
				t.Fatalf("resolve adapter: %v", err)
			}
			_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
				Source:  adapter.ProvisionSource{Kind: "oracle_datapump", Path: path},
				Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
			}, sbx)
			var perr *adapter.Error
			if !errors.As(err, &perr) || perr.Code != tt.wantCode || !strings.Contains(perr.Message, tt.wantMsg) {
				t.Fatalf("provision error = %v, want %s carrying %q", err, tt.wantCode, tt.wantMsg)
			}
			t.Logf("%s: %s", tt.name, perr.Message)
		})
	}
}

// TestSandboxShapeIsRefusedByTheEnginesOwnWords pins the two sandbox
// mistakes the README warns about, each answered with the instruction
// that fixes it: an instance the image already started (the sandbox was
// not idle), and a loopback-only sandbox (the provider's default
// network), which the instance refuses at the IPC layer.
func TestSandboxShapeIsRefusedByTheEnginesOwnWords(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	network := internalNetwork(t, ctx)
	provider := docker.New(nil)
	dump := filepath.Join(t.TempDir(), "orders.dmp")
	if err := os.WriteFile(dump, []byte("the instance never gets this far"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("an instance already running", func(t *testing.T) {
		sbx, err := provider.Create(ctx, sandboxParams(image, network))
		if err != nil {
			t.Fatalf("create sandbox: %v", err)
		}
		defer destroy(t, sbx)
		if out := execScript(t, ctx, sbx, productionStart+"echo started"); !strings.Contains(out, "started") {
			t.Fatalf("production start: %s", out)
		}
		assertRefusal(t, ctx, sbx, dump, "invalid_request", "sleep infinity")
	})

	t.Run("loopback only", func(t *testing.T) {
		params := sandboxParams(image, network)
		params["network"] = "none"
		sbx, err := provider.Create(ctx, params)
		if err != nil {
			t.Fatalf("create sandbox: %v", err)
		}
		defer destroy(t, sbx)
		assertRefusal(t, ctx, sbx, dump, "invalid_request", "docker network create --internal")
	})
}

func assertRefusal(t *testing.T, ctx context.Context, sbx *docker.Sandbox, dump, wantCode, wantMsg string) {
	t.Helper()
	runner, err := adapter.New("oracle", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "oracle_datapump", Path: dump},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	var perr *adapter.Error
	if !errors.As(err, &perr) || perr.Code != wantCode || !strings.Contains(perr.Message, wantMsg) {
		t.Fatalf("provision error = %v, want %s carrying %q", err, wantCode, wantMsg)
	}
	t.Logf("refused: %s", perr.Message)
}

// seedFixture runs a seed script in a fresh sandbox and extracts the
// named paths to the host.
func seedFixture(t *testing.T, ctx context.Context, provider *docker.Provider, image, network, script string,
	extract map[string]string) {
	t.Helper()
	seed, err := provider.Create(ctx, sandboxParams(image, network))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)

	res, err := seed.Exec(ctx, sandbox.ExecRequest{Argv: []string{"bash", "-c", script}})
	if err != nil {
		t.Fatalf("seed exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("seed fixture: exit %d: %s %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	for src, dest := range extract {
		if out, err := exec.CommandContext(ctx, "docker", "cp", seed.ID()+":"+src, dest).CombinedOutput(); err != nil {
			t.Fatalf("extract fixture: %v: %s", err, out)
		}
	}
}

// copyIntoSandbox places a host tree inside a sandbox the test drives by
// hand (the control instance), readable by the engine's user. docker cp
// keeps the host's ownership, which is whatever uid runs the test — so
// the permissions are opened as root, the one user the image's uid
// 54321 cannot stand in for.
func copyIntoSandbox(t *testing.T, ctx context.Context, sbx *docker.Sandbox, src, dest string) {
	t.Helper()
	if out, err := exec.CommandContext(ctx, "docker", "cp", src, sbx.ID()+":"+dest).CombinedOutput(); err != nil {
		t.Fatalf("copy fixture in: %v: %s", err, out)
	}
	if out, err := exec.CommandContext(ctx, "docker", "exec", "-u", "0", sbx.ID(),
		"chmod", "-R", "a+rwX", dest).CombinedOutput(); err != nil {
		t.Fatalf("open fixture permissions: %v: %s", err, out)
	}
}

// execScript runs one bash script in the sandbox and returns its output.
func execScript(t *testing.T, ctx context.Context, sbx *docker.Sandbox, script string) string {
	t.Helper()
	res, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{"bash", "-c", script}})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exec: exit %d: %s %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	return string(res.Stdout)
}

// buildAdapterOnPath builds the adapter binary and puts it on PATH under
// its protocol name.
func buildAdapterOnPath(t *testing.T, ctx context.Context) {
	t.Helper()
	binDir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o",
		filepath.Join(binDir, "probavi-adapter-oracle"), ".").CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func destroy(t *testing.T, sbx *docker.Sandbox) {
	t.Helper()
	if err := sbx.Destroy(context.Background()); err != nil {
		t.Errorf("destroy sandbox: %v", err)
	}
}
