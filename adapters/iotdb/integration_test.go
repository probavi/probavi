//go:build integration

package main_test

import (
	"context"
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
	"github.com/probavi/probavi/internal/checks"
	"github.com/probavi/probavi/internal/config"
	"github.com/probavi/probavi/internal/sandbox"
	"github.com/probavi/probavi/internal/sandbox/docker"
)

// engineMemoryLimit caps every container this suite starts. The adapter's
// 4 MiB WAL buffer is what lets the fixture's regions start inside it.
const engineMemoryLimit = "1g"

const (
	// rows is how many points each tree device and the table hold.
	rows = 2000
	// deleted is how many of d1's temperatures the fixture deletes, so a
	// restore that brought deleted points back would count them.
	deleted = 100
	// passwordEnv is where the suite hands the adapter the password the
	// fixture's root user was given — not IoTDB's default, because an
	// offline copy keeps the password of the node it was taken from.
	passwordEnv = "IOTDB_IT_ROOT_PASSWORD"
	password    = "Drill-Pass-9"
)

// base is the fixture's first tree timestamp. It is fixed rather than taken
// from the clock so the fixture's data files have the same bytes on every
// run, which the damage test depends on.
var base = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).UnixMilli()

// verifiedImage is the engine image this run restores from: the manifest's
// baseline, or the version-matrix job's PROBAVI_IT_IMAGE when it names one
// the manifest already lists (docs/engine-versions.md §2).
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

// hasTableModel reports whether the image's engine is 2.0 or later.
func hasTableModel(t *testing.T, image string) bool {
	t.Helper()
	tag := image[strings.LastIndex(image, ":")+1:]
	major, err := strconv.Atoi(strings.SplitN(tag, ".", 2)[0])
	if err != nil {
		t.Fatalf("read the engine version from %s: %v", image, err)
	}
	return major >= 2
}

func sandboxParams(image string) map[string]string {
	return map[string]string{"image": image, "command": "sleep infinity", "memory": engineMemoryLimit}
}

func buildAdapterOnPath(t *testing.T, ctx context.Context) {
	t.Helper()
	binDir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o",
		filepath.Join(binDir, "probavi-adapter-iotdb"), ".").CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func destroy(t *testing.T, sbx *docker.Sandbox) {
	t.Helper()
	if err := sbx.Destroy(context.WithoutCancel(t.Context())); err != nil {
		t.Errorf("destroy sandbox: %v", err)
	}
}

// seedScript starts a node, fills it, and stops it — the state an offline
// copy is taken in.
//
// $1 is the root password to set, $2 "table" to add the table model, $3
// "ttl" to add a database whose TTL hides every row it holds, $4 a host name
// to configure the node with instead of loopback.
const seedScript = `set -eu
pw=$1 table=$2 ttl=$3 host=${4:-127.0.0.1}
if [ "$host" != 127.0.0.1 ]; then
  echo "127.0.0.1 $host" >> /etc/hosts
  conf=/iotdb/conf/iotdb-system.properties
  for k in cn_internal_address dn_internal_address dn_rpc_address; do sed -i "s/^$k=.*/$k=$host/" "$conf"; done
  sed -i "s/^cn_seed_config_node=.*/cn_seed_config_node=$host:10710/; s/^dn_seed_config_node=.*/dn_seed_config_node=$host:10710/" "$conf"
fi
nohup start-confignode.sh >/tmp/cn.out 2>&1 </dev/null &
nohup start-datanode.sh >/tmp/dn.out 2>&1 </dev/null &
current=root
cli() { start-cli.sh -h "$host" -pw "$current" "$@" 2>&1; }
must() {
  local out
  out=$(cli "$@") || true
  printf '%s\n' "$out" | grep -qi success || { printf 'refused: %s\n%s\n' "${*: -1}" "$out" >&2; exit 1; }
}
for i in $(seq 1 240); do cli -e "show cluster" | grep -q 'DataNode|Running' && break; sleep 1; done
for i in $(seq 1 120); do cli -e "create database root.rig" | grep -qi 'success' && break; sleep 1; done
must -e "alter user root set password '$pw'"
current=$pw
d1= d2=
for i in $(seq 0 $((` + "ROWS" + ` - 1))); do
  t=$((` + "BASE" + ` + i * 1000)); note="n$i"
  [ "$i" = 7 ] && note=' a | b '
  d1="$d1($t, $i.5, '$note'),"; d2="$d2($t, $i, 'm$i'),"
done
must -e "insert into root.rig.d1(time, temperature, note) values ${d1%,}"
must -e "create aligned timeseries root.rig.d2(v INT32, note TEXT)"
must -e "insert into root.rig.d2(time, v, note) aligned values ${d2%,}"
must -e "delete from root.rig.d1.temperature where time < $((` + "BASE" + ` + ` + "DELETED" + ` * 1000))"
if [ "$table" = table ]; then
  now=$(date +%s%3N) rowsql=
  for i in $(seq 0 $((` + "ROWS" + ` - 1))); do rowsql="$rowsql($((now - (` + "ROWS" + ` - i) * 1000)), 'p$((i % 5))', 'model-$((i % 3))', $i.25),"; done
  must -sql_dialect table -e "create database rig_t"
  must -sql_dialect table -e "create table rig_t.sensors (plant STRING TAG, model STRING ATTRIBUTE, temperature FLOAT FIELD)"
  must -sql_dialect table -e "insert into rig_t.sensors(time, plant, model, temperature) values ${rowsql%,}"
fi
if [ "$ttl" = ttl ]; then
  must -e "insert into root.ttl.d(time, v) values (1577836800000, 1), (1577836801000, 2)"
  must -e "set ttl to root.ttl.** 86400000"
fi
must -e "flush"
stop-standalone.sh >/dev/null 2>&1 || true
for i in $(seq 1 120); do pgrep -x java >/dev/null || break; sleep 1; done
echo seeded`

// fixture is one seeded node's offline copy on the host.
type fixture struct {
	table, ttl bool
	host       string
}

// seed produces an offline copy on the host the way the README tells an
// operator to: fill a node, stop it, copy its data directory.
func (f fixture) seed(t *testing.T, ctx context.Context, provider *docker.Provider, image string) string {
	t.Helper()
	// The seed node runs the image's own configuration, WAL buffer
	// included, and at the drill's 1 GiB it cannot create a third data
	// region (measured: the table and TTL fixtures failed there). The
	// drills run at 1 GiB on the adapter's configuration.
	params := sandboxParams(image)
	params["memory"] = "2g"
	sbx, err := provider.Create(ctx, params)
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, sbx)
	script := strings.NewReplacer("ROWS", strconv.Itoa(rows), "BASE", strconv.FormatInt(base, 10),
		"DELETED", strconv.Itoa(deleted)).Replace(seedScript)
	mode := func(on bool, word string) string {
		if on {
			return word
		}
		return "-"
	}
	res, err := sbx.Exec(ctx, sandbox.ExecRequest{
		Argv: []string{"bash", "-c", script, "bash", password, mode(f.table, "table"), mode(f.ttl, "ttl"), f.host},
	})
	if err != nil || res.ExitCode != 0 || !strings.Contains(string(res.Stdout), "seeded") {
		t.Fatalf("seed IoTDB: %v exit %d: %s%s", err, res.ExitCode, res.Stdout, res.Stderr)
	}
	dest := filepath.Join(t.TempDir(), "backup")
	if err := os.MkdirAll(dest, 0o750); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.CommandContext(ctx, "docker", "cp", sbx.ID()+":/iotdb/data", dest).CombinedOutput(); err != nil {
		t.Fatalf("copy the data directory out: %v: %s", err, out)
	}
	return dest
}

// drill provisions one copy and returns the result or the adapter's
// refusal.
func drill(t *testing.T, ctx context.Context, provider *docker.Provider, image string, source adapter.ProvisionSource, options map[string]string) (*adapter.ProvisionResult, *docker.Sandbox, *adapter.Runner, error) {
	t.Helper()
	runner, err := adapter.New("iotdb", nil, &adapter.Options{CredentialEnv: []string{passwordEnv}})
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	t.Cleanup(func() { destroy(t, sbx) })
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source: source, Options: options,
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	return res, sbx, runner, err
}

// withPassword is the source and options that name the fixture's password.
func withPassword(kind, path string) (adapter.ProvisionSource, map[string]string) {
	return adapter.ProvisionSource{Kind: kind, Path: path, CredentialEnv: []string{passwordEnv}},
		map[string]string{"password_env": passwordEnv}
}

// runChecks runs checks the way the core does, through the runner the
// adapter declares.
func runChecks(t *testing.T, ctx context.Context, runner *adapter.Runner, sbx *docker.Sandbox, res *adapter.ProvisionResult, list []config.Check) []checks.Result {
	t.Helper()
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	results, err := checks.Run(ctx, list, checks.Deps{
		Exec:   sbx,
		Runner: checks.Runner{Argv: probe.SQLRunner.Argv, Env: probe.SQLRunner.Env},
		Target: checks.Target{User: res.Connection.User, Database: res.Connection.Database, Password: password},
	})
	if err != nil {
		t.Fatalf("checks.Run: %v", err)
	}
	return results
}

func stamp(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z")
}

func TestEndToEndRestoreDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	t.Setenv(passwordEnv, password)
	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)
	tables := hasTableModel(t, image)
	backup := fixture{table: tables}.seed(t, ctx, provider, image)

	archive := filepath.Join(t.TempDir(), "backup.tar.gz")
	if out, err := exec.CommandContext(ctx, "tar", "-czf", archive, "-C", backup, "data").CombinedOutput(); err != nil {
		t.Fatalf("make the archive fixture: %v: %s", err, out)
	}

	t.Run("the tree dialect, from the directory", func(t *testing.T) {
		source, options := withPassword("iotdb_data", backup)
		res, sbx, runner, err := drill(t, ctx, provider, image, source, options)
		if err != nil {
			t.Fatalf("provision: %v", err)
		}
		if res.Timings.RestoreSeconds <= 0 || res.Timings.EngineReadySeconds <= 0 {
			t.Errorf("timings = %+v, want real measurements", res.Timings)
		}
		if !strings.HasPrefix(res.SourceIdentity.Checksum, "sha256:") || res.SourceIdentity.SizeBytes == 0 || res.SourceIdentity.CreatedAt != nil {
			t.Errorf("source identity = %+v, want a checksum and no creation time", res.SourceIdentity)
		}
		if health, err := runner.Healthcheck(ctx, &res.Connection, res.State, sbx); err != nil || !health.Healthy {
			t.Fatalf("healthcheck: %+v, %v", health, err)
		}
		results := runChecks(t, ctx, runner, sbx, res, []config.Check{
			{Name: "notes", SQL: "select count(note) from root.rig.d1", Expect: config.ScalarFromString(strconv.Itoa(rows))},
			{Name: "deletions stay deleted", SQL: "select count(temperature) from root.rig.d1", Expect: config.ScalarFromString(strconv.Itoa(rows - deleted))},
			{Name: "the aligned device", SQL: "select count(v) from root.rig.d2", Expect: config.ScalarFromString(strconv.Itoa(rows))},
			// A value holding the CLI's own column separator and the spaces
			// it trims comes back exactly, with its time rendered as RFC 3339.
			{Name: "a value the CLI would mangle", SQL: fmt.Sprintf("select note from root.rig.d1 where time = %d", base+7000), Expect: config.ScalarFromString(stamp(base+7000) + "\t a | b")},
		})
		for _, r := range results {
			if !r.OK {
				t.Errorf("check %s: %s", r.Name, r.Detail)
			}
		}
	})

	t.Run("from the archive", func(t *testing.T) {
		source, options := withPassword("iotdb_data_tar", archive)
		if tables {
			options["database"] = "rig_t"
		}
		res, sbx, runner, err := drill(t, ctx, provider, image, source, options)
		if err != nil {
			t.Fatalf("provision: %v", err)
		}
		if !tables {
			if results := runChecks(t, ctx, runner, sbx, res, []config.Check{
				{Name: "notes", SQL: "select count(note) from root.rig.d2", Expect: config.ScalarFromString(strconv.Itoa(rows))},
			}); !results[0].OK {
				t.Errorf("check: %s", results[0].Detail)
			}
			return
		}
		one, all := int64(1), int64(rows)
		results := runChecks(t, ctx, runner, sbx, res, []config.Check{
			{Builtin: config.CheckTableExists, Table: "rig_t.sensors"},
			{Builtin: config.CheckRowCount, Table: "rig_t.sensors", Min: &all, Max: &all},
			{Builtin: config.CheckFreshness, Table: "rig_t.sensors", Column: "time", MaxAge: config.Duration(2 * time.Hour)},
			{Name: "attributes survive", SQL: "SELECT count(DISTINCT model) FROM sensors", Expect: config.ScalarFromString("3")},
			{Builtin: config.CheckTableExists, Table: "rig_t.nosuch"},
			{Builtin: config.CheckRowCount, Table: "rig_t.sensors", Min: &one, Max: &one},
			{Builtin: config.CheckFreshness, Table: "rig_t.sensors", Column: "time", MaxAge: config.Duration(time.Millisecond)},
		})
		for i, want := range []bool{true, true, true, true, false, false, false} {
			if results[i].OK != want {
				t.Errorf("check %d (%s) = ok:%v %q, want ok:%v", i, results[i].Name, results[i].OK, results[i].Detail, want)
			}
		}
		// The failures must be the bounds', not the dialect's.
		for i, want := range map[int]string{5: "rows", 6: "old"} {
			if !strings.Contains(results[i].Detail, want) {
				t.Errorf("check %d detail = %q, want it to mention %q", i, results[i].Detail, want)
			}
		}
	})

	t.Run("the default password is refused by name", func(t *testing.T) {
		_, _, _, err := drill(t, ctx, provider, image, adapter.ProvisionSource{Kind: "iotdb_data", Path: backup}, nil)
		wantRefusal(t, err, "invalid_request", "IoTDB's default password")
	})
}

// TestDamageIsRefusedAtTheFullRead is this adapter's reason to read every
// value: the damage below leaves the engine starting normally and its
// statistics answering as if nothing happened.
func TestDamageIsRefusedAtTheFullRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	t.Setenv(passwordEnv, password)
	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)
	backup := fixture{}.seed(t, ctx, provider, image)

	for _, tc := range []struct {
		name   string
		damage func(t *testing.T, file string)
	}{
		{"a truncated data file", func(t *testing.T, file string) {
			info, err := os.Stat(file)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(file, info.Size()*6/10); err != nil {
				t.Fatal(err)
			}
		}},
		// One byte at this offset breaks a page of the fixture in both
		// verified lines. Not every byte does — a change that still decodes
		// reads back as other values, which the README states — so the
		// offset is a measured one rather than a claim about all of them.
		{"a changed byte", func(t *testing.T, file string) { changeByte(t, file, 6, 13) }},
		{"a damaged span", func(t *testing.T, file string) { damageSpan(t, file, 4096) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			damaged := filepath.Join(t.TempDir(), "backup")
			if out, err := exec.CommandContext(ctx, "cp", "-a", backup, damaged).CombinedOutput(); err != nil {
				t.Fatalf("copy the fixture: %v: %s", err, out)
			}
			tc.damage(t, rigDataFile(t, damaged))
			source, options := withPassword("iotdb_data", damaged)
			_, _, _, err := drill(t, ctx, provider, image, source, options)
			wantRefusal(t, err, "source_corrupt", "could not read every value")
		})
	}
}

// rigDataFile is the largest data file of the fixture's tree database.
func rigDataFile(t *testing.T, backup string) string {
	t.Helper()
	var largest string
	var size int64
	root := filepath.Join(backup, "data", "datanode", "data", "sequence", "root.rig")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".tsfile") {
			return err
		}
		info, err := d.Info()
		if err == nil && info.Size() > size {
			largest, size = path, info.Size()
		}
		return err
	})
	if err != nil || largest == "" {
		t.Fatalf("find the fixture's data file under %s: %v", root, err)
	}
	return largest
}

// changeByte XORs the byte at numerator/denominator of the file.
func changeByte(t *testing.T, file string, numerator, denominator int64) {
	t.Helper()
	data, err := os.ReadFile(file) //nolint:gosec // G304: the test's own fixture
	if err != nil {
		t.Fatal(err)
	}
	data[int64(len(data))*numerator/denominator] ^= 0x5a
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// damageSpan XORs n bytes from the middle of the file: a stretch of a data
// file the way a bad disk sector or an interrupted copy leaves one.
func damageSpan(t *testing.T, file string, n int) {
	t.Helper()
	data, err := os.ReadFile(file) //nolint:gosec // G304: the test's own fixture
	if err != nil {
		t.Fatal(err)
	}
	start := len(data)/2 - n/2
	for i := start; i < start+n && i < len(data); i++ {
		data[i] ^= 0x5a
	}
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestATTLThatHidesEverythingIsRefused pins the data-lifecycle fence: rows
// older than the TTL their scope carries are hidden when a query runs, so
// the drill is refused naming both rather than reported green.
func TestATTLThatHidesEverythingIsRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	t.Setenv(passwordEnv, password)
	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)
	backup := fixture{ttl: true}.seed(t, ctx, provider, image)

	source, options := withPassword("iotdb_data", backup)
	_, _, _, err := drill(t, ctx, provider, image, source, options)
	wantRefusal(t, err, "restore_failed", "root.ttl.** holds data and reads no row: the copy carries a TTL of 24h0m0s")
}

// TestANodeAddressedByNameStarts pins the host-name route: the copy records
// the name, and the sandbox has to both name it and resolve it to loopback.
func TestANodeAddressedByNameStarts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	t.Setenv(passwordEnv, password)
	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)
	backup := fixture{host: "iotdb-prod-1"}.seed(t, ctx, provider, image)

	source, options := withPassword("iotdb_data", backup)
	res, sbx, runner, err := drill(t, ctx, provider, image, source, options)
	if err != nil {
		t.Fatalf("provision a host-named copy: %v", err)
	}
	if results := runChecks(t, ctx, runner, sbx, res, []config.Check{
		{Name: "notes", SQL: "select count(note) from root.rig.d1", Expect: config.ScalarFromString(strconv.Itoa(rows))},
	}); !results[0].OK {
		t.Errorf("check: %s", results[0].Detail)
	}
}

func wantRefusal(t *testing.T, err error, code, says string) {
	t.Helper()
	var aerr *adapter.Error
	if !errors.As(err, &aerr) {
		t.Fatalf("got %v, want the adapter's %s", err, code)
	}
	if aerr.Code != code || !strings.Contains(aerr.Message, says) {
		t.Errorf("got %s %q, want %s saying %q", aerr.Code, aerr.Message, code, says)
	}
}
