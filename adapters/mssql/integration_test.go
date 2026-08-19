//go:build integration

package main_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/probavi/probavi/internal/adapter"
	"github.com/probavi/probavi/internal/capabilities"
	"github.com/probavi/probavi/internal/sandbox"
	"github.com/probavi/probavi/internal/sandbox/docker"
)

const (
	// seedPassword is only for the throwaway seed engine this test starts
	// to produce a fixture; the drill engine uses the adapter's documented
	// sandbox constant.
	seedPassword = "Probavi!Seed0"

	// appLoginPassword seeds the application login the logins-drill
	// fixture exports; it exists only inside the test sandboxes. The
	// exported script carries its SCRAM-free password *hash*, so a
	// successful authentication with this plaintext proves the hash and
	// SID round-tripped through the drill.
	appLoginPassword = "AppSeed!OnlyInSandbox1"

	// engineMemoryLimit caps every engine this suite starts. SQL Server
	// refuses to start below 2 GB, and the fixtures never need more.
	engineMemoryLimit = "2g"
)

// orphanedUsersSQL is this test's own statement of the defect — kept
// independent of the adapter's internal query so a regression there cannot
// blind the assertion.
const orphanedUsersSQL = "SET NOCOUNT ON; SELECT dp.name FROM sys.database_principals dp " +
	"LEFT JOIN sys.server_principals sp ON dp.sid = sp.sid " +
	"WHERE dp.type = 'S' AND dp.authentication_type = 1 AND sp.sid IS NULL ORDER BY dp.name"

// verifiedImage is the engine image this run restores from: the manifest's
// baseline, or the version-matrix job's PROBAVI_IT_IMAGE when it names one
// the manifest already lists. The manifest and this suite read the same
// values, so docs/capabilities.json can never claim an engine version CI
// does not actually restore from (docs/capabilities.md §1,
// docs/engine-versions.md §2).
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

// sandboxParams returns the documented drill-config sandbox params: the
// image starts idle (SQL Server cannot run without a superuser password,
// and a password in sandbox params would enter the signed evidence record
// — so the adapter starts and owns the engine).
//
// The memory limit is not incidental. Left unbounded, SQL Server sizes its
// buffer pool against the whole host and several engines in one suite run
// can push a developer's machine into swapping or the OOM killer; the
// fixtures here are a handful of rows, so the documented minimum is
// plenty and keeps the suite's footprint predictable.
func sandboxParams(t *testing.T) map[string]string {
	return map[string]string{
		"image":   verifiedImage(t),
		"command": "sleep infinity",
		"memory":  engineMemoryLimit,
	}
}

// TestEndToEndRestoreDrill proves the fourth engine through the unchanged
// core: the docker provider, the core-side protocol client, and this
// adapter — as separate processes — restore a genuine BACKUP DATABASE
// artifact under a new name with server-side MOVEs, and validate it
// through the probe-declared sql_runner (QUOTED_IDENTIFIER + NOCOUNT
// bridges included). It also exercises put_file against a non-root image
// user for the first time.
func TestEndToEndRestoreDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	provider := docker.New(nil)

	fixture := filepath.Join(t.TempDir(), "shop.bak")
	makeFixture(t, ctx, provider, fixture)

	sbx, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("mssql", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}

	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.Name != "mssql" || len(probe.SQLRunner.Argv) == 0 {
		t.Fatalf("probe = %+v", probe)
	}

	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "bak", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if res.Timings.RestoreSeconds <= 0 || res.Timings.EngineReadySeconds <= 0 {
		t.Errorf("timings = %+v, want real measurements", res.Timings)
	}
	if !strings.HasPrefix(res.SourceIdentity.Checksum, "sha256:") || res.SourceIdentity.SizeBytes == 0 {
		t.Errorf("source identity = %+v", res.SourceIdentity)
	}
	if res.Connection.Database != "probavi" || res.Connection.User != "sa" {
		t.Errorf("connection = %+v, want sa on the default restore target", res.Connection)
	}

	health, err := runner.Healthcheck(ctx, &res.Connection, res.State, sbx)
	if err != nil {
		t.Fatalf("healthcheck: %v", err)
	}
	if !health.Healthy {
		t.Fatalf("healthcheck = %+v, want healthy", health)
	}

	// Validate the restored data through the probe-declared sql_runner —
	// exactly how internal/checks runs checks without engine knowledge.
	// The double-quoted identifier pins the -I bridge; the clean numeric
	// output pins the SQLCMDINI NOCOUNT bridge.
	argv := make([]string, 0, len(probe.SQLRunner.Argv))
	for _, a := range probe.SQLRunner.Argv {
		a = strings.ReplaceAll(a, "{{user}}", res.Connection.User)
		a = strings.ReplaceAll(a, "{{database}}", res.Connection.Database)
		a = strings.ReplaceAll(a, "{{sql}}", `SELECT count(*) FROM "orders"`)
		argv = append(argv, a)
	}
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: argv, Env: probe.SQLRunner.Env})
	if err != nil {
		t.Fatalf("sql_runner exec: %v", err)
	}
	if count := strings.TrimSpace(string(out.Stdout)); out.ExitCode != 0 || count != "500" {
		t.Fatalf("row count = %q (exit %d, stderr %s), want exactly 500 with no decoration",
			count, out.ExitCode, out.Stderr)
	}

	teardown, err := runner.Teardown(ctx, res.State, "completed", sbx)
	if err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if !teardown.Released {
		t.Errorf("teardown = %+v", teardown)
	}
}

// TestCorruptBakVerdict proves a broken backup yields the right verdict
// through the whole stack, not a generic failure.
func TestCorruptBakVerdict(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)

	corrupt := filepath.Join(t.TempDir(), "corrupt.bak")
	garbage := make([]byte, 64*1024)
	if _, err := rand.Read(garbage); err != nil {
		t.Fatalf("garbage: %v", err)
	}
	if err := os.WriteFile(corrupt, garbage, 0o600); err != nil {
		t.Fatalf("write corrupt fixture: %v", err)
	}

	provider := docker.New(nil)
	sbx, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("mssql", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "bak", Path: corrupt},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) || aerr.Code != "source_corrupt" {
		t.Fatalf("provision error = %v, want source_corrupt", err)
	}
}

// TestLoginsDrillEndToEnd reproduces issue #87 and proves the fix, and
// each half is worthless without the other. A .bak carries database users
// but not the server logins they map to by SID, so a plain bak drill
// passes while the application principal cannot log in — the record claims
// more than the drill proved. The bak_with_logins kind replays an exported
// logins script first and gates on orphaned users afterwards.
func TestLoginsDrillEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	provider := docker.New(nil)
	fixtureDir := makeGrantedFixture(t, ctx, provider)

	runner, err := adapter.New("mssql", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}

	t.Run("kind bak still passes and leaves the orphan", func(t *testing.T) {
		assertBakLeavesOrphan(t, ctx, provider, runner, fixtureDir)
	})
	t.Run("kind bak_with_logins restores the principal chain", func(t *testing.T) {
		assertWithLoginsRestoresChain(t, ctx, provider, runner, fixtureDir)
	})
	t.Run("an incomplete logins script fails the drill", func(t *testing.T) {
		assertIncompleteScriptFails(t, ctx, provider, runner, fixtureDir)
	})
}

// assertBakLeavesOrphan is the reproduction half: the plain bak drill
// passes while the restored user is orphaned and its login cannot
// authenticate — the premise of #87.
func assertBakLeavesOrphan(t *testing.T, ctx context.Context, provider *docker.Provider, runner *adapter.Runner, fixtureDir string) {
	sbx, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer destroy(t, sbx)

	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "bak", Path: filepath.Join(fixtureDir, "shop.bak")},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if orphans := orphanedUsers(t, ctx, sbx, res.Connection.Database); !slices.Contains(orphans, "app_user") {
		t.Errorf("orphaned users = %v, want app_user — the premise of #87", orphans)
	}
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{
		Argv: appLoginArgv(res.Connection.Database, "SELECT 1"),
		Env:  map[string]string{"SQLCMDPASSWORD": appLoginPassword},
	})
	if err != nil {
		t.Fatalf("app login exec: %v", err)
	}
	if out.ExitCode == 0 {
		t.Error("app_login authenticated against a restore without logins — premise broken")
	}
}

// assertWithLoginsRestoresChain is the fix half, with the strongest proof
// there is: the restored login authenticates with its original password
// (the hash round-tripped) and reads exactly the table its restored grant
// covers.
func assertWithLoginsRestoresChain(t *testing.T, ctx context.Context, provider *docker.Provider, runner *adapter.Runner, fixtureDir string) {
	sbx, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer destroy(t, sbx)

	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source: adapter.ProvisionSource{
			Kind:   "bak_with_logins",
			Path:   fixtureDir,
			Params: map[string]string{"logins": "logins.sql", "bak": "shop.bak"},
		},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if res.Timings.RestoreSeconds <= 0 {
		t.Errorf("timings = %+v, want real measurements", res.Timings)
	}
	if orphans := orphanedUsers(t, ctx, sbx, res.Connection.Database); len(orphans) != 0 {
		t.Errorf("orphaned users = %v, want none", orphans)
	}
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{
		Argv: appLoginArgv(res.Connection.Database, "SET NOCOUNT ON; SELECT count(*) FROM dbo.orders"),
		Env:  map[string]string{"SQLCMDPASSWORD": appLoginPassword},
	})
	if err != nil {
		t.Fatalf("app login exec: %v", err)
	}
	if count := strings.TrimSpace(string(out.Stdout)); out.ExitCode != 0 || count != "1" {
		t.Errorf("app_login count = %q (exit %d, stderr %s), want 1 row through the restored grant",
			count, out.ExitCode, out.Stderr)
	}
}

// assertIncompleteScriptFails proves the orphan gate bites: a script that
// loads cleanly but covers nothing must fail the drill, or an incomplete
// export would reintroduce the defect one level down.
func assertIncompleteScriptFails(t *testing.T, ctx context.Context, provider *docker.Provider, runner *adapter.Runner, fixtureDir string) {
	incomplete := t.TempDir()
	copyFile(t, filepath.Join(fixtureDir, "shop.bak"), filepath.Join(incomplete, "shop.bak"))
	if err := os.WriteFile(filepath.Join(incomplete, "logins.sql"), []byte("PRINT 'no logins here';\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	sbx, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer destroy(t, sbx)

	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source: adapter.ProvisionSource{
			Kind:   "bak_with_logins",
			Path:   incomplete,
			Params: map[string]string{"logins": "logins.sql", "bak": "shop.bak"},
		},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) || aerr.Code != "restore_failed" {
		t.Fatalf("provision error = %v, want restore_failed from the orphan gate", err)
	}
	if !strings.Contains(aerr.Message, "no matching server login") {
		t.Errorf("message = %q, want the orphan verdict", aerr.Message)
	}
}

// orphanedUsers lists restored SQL users with no matching server login,
// asked through the drill engine as the sandbox superuser.
func orphanedUsers(t *testing.T, ctx context.Context, sbx *docker.Sandbox, database string) []string {
	t.Helper()
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{
		Argv: []string{"/opt/mssql-tools18/bin/sqlcmd", "-S", "127.0.0.1,1433", "-U", "sa",
			"-C", "-b", "-l", "5", "-d", database, "-h", "-1", "-W", "-Q", orphanedUsersSQL},
		Env: map[string]string{"SQLCMDPASSWORD": "Probavi!DrillSandbox0"},
	})
	if err != nil || out.ExitCode != 0 {
		t.Fatalf("orphan query: %v (exit %d, stderr %s)", err, out.ExitCode, out.Stderr)
	}
	var names []string
	for _, line := range strings.Split(string(out.Stdout), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	return names
}

func appLoginArgv(database, query string) []string {
	return []string{"/opt/mssql-tools18/bin/sqlcmd", "-S", "127.0.0.1,1433", "-U", "app_login",
		"-C", "-b", "-l", "5", "-d", database, "-h", "-1", "-W", "-Q", query}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// makeGrantedFixture seeds a throwaway engine with an application login, a
// database user mapped to it, and a granted table; it extracts a real
// BACKUP DATABASE artifact plus a logins script exported the way operators
// do it — CREATE LOGIN with the password hash and the original SID, from
// sys.sql_logins — into one source directory.
func makeGrantedFixture(t *testing.T, ctx context.Context, provider *docker.Provider) string {
	t.Helper()
	dir := t.TempDir()
	seed, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)

	startEngine(t, ctx, seed, seedPassword)
	awaitReady(t, ctx, seed, seedPassword)
	seedSQL := []string{
		"CREATE LOGIN app_login WITH PASSWORD = '" + appLoginPassword + "', CHECK_POLICY = OFF",
		"CREATE DATABASE shop",
		"USE shop; CREATE USER app_user FOR LOGIN app_login",
		"CREATE TABLE shop.dbo.orders (id INT PRIMARY KEY, total DECIMAL(10,2))",
		"INSERT INTO shop.dbo.orders VALUES (1, 10.50)",
		"USE shop; GRANT SELECT ON dbo.orders TO app_user",
		"BACKUP DATABASE shop TO DISK = N'/tmp/shop.bak'",
	}
	for _, stmt := range seedSQL {
		mustSQL(t, ctx, seed, seedPassword, stmt)
	}

	// PRINT instead of SELECT sidesteps sqlcmd's column-width formatting;
	// with -r 0 only the printed line reaches stdout.
	export, err := seed.Exec(ctx, sandbox.ExecRequest{
		Argv: []string{"/opt/mssql-tools18/bin/sqlcmd", "-S", "127.0.0.1,1433", "-U", "sa",
			"-C", "-b", "-r", "0", "-Q",
			"SET NOCOUNT ON; DECLARE @s varchar(max); " +
				"SELECT @s = 'CREATE LOGIN [' + name + '] WITH PASSWORD = ' + " +
				"CONVERT(varchar(max), LOGINPROPERTY(name, 'PasswordHash'), 1) + " +
				"' HASHED, SID = ' + CONVERT(varchar(max), sid, 1) + ', CHECK_POLICY = OFF;' " +
				"FROM sys.sql_logins WHERE name = 'app_login'; PRINT @s"},
		Env: map[string]string{"SQLCMDPASSWORD": seedPassword},
	})
	if err != nil || export.ExitCode != 0 {
		t.Fatalf("export logins: %v (exit %d, stderr %s)", err, export.ExitCode, export.Stderr)
	}
	script := strings.TrimSpace(string(export.Stdout))
	if !strings.Contains(script, "HASHED") || !strings.Contains(script, "SID = 0x") {
		t.Fatalf("exported script lacks hash or SID: %s", script)
	}
	if err := os.WriteFile(filepath.Join(dir, "logins.sql"), []byte(script+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if out, err := exec.CommandContext(ctx, "docker", "cp", seed.ID()+":/tmp/shop.bak",
		filepath.Join(dir, "shop.bak")).CombinedOutput(); err != nil {
		t.Fatalf("extract fixture: %v: %s", err, out)
	}
	return dir
}

// buildAdapterOnPath builds the adapter binary and puts it on PATH under
// its protocol name.
func buildAdapterOnPath(t *testing.T, ctx context.Context) {
	t.Helper()
	binDir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o",
		filepath.Join(binDir, "probavi-adapter-mssql"), ".").CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// makeFixture seeds a throwaway engine with 500 rows and extracts a real
// BACKUP DATABASE artifact to the host.
func makeFixture(t *testing.T, ctx context.Context, provider *docker.Provider, dest string) {
	t.Helper()
	seed, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)

	startEngine(t, ctx, seed, seedPassword)
	awaitReady(t, ctx, seed, seedPassword)
	seedSQL := []string{
		"CREATE DATABASE shop",
		"CREATE TABLE shop.dbo.orders (id INT IDENTITY PRIMARY KEY, total DECIMAL(10,2) NOT NULL)",
		"INSERT INTO shop.dbo.orders (total) SELECT TOP 500 ROUND(RAND(CHECKSUM(NEWID()))*100,2) FROM sys.all_columns",
		"BACKUP DATABASE shop TO DISK = N'/tmp/shop.bak'",
	}
	for _, stmt := range seedSQL {
		mustSQL(t, ctx, seed, seedPassword, stmt)
	}

	// The provider deliberately has no get-file verb; pulling the fixture
	// out of the seed container is test harness work, done with the CLI.
	if out, err := exec.CommandContext(ctx, "docker", "cp", seed.ID()+":/tmp/shop.bak", dest).CombinedOutput(); err != nil {
		t.Fatalf("extract fixture: %v: %s", err, out)
	}
}

// startEngine launches sqlservr in an idle container the same way the
// adapter does.
func startEngine(t *testing.T, ctx context.Context, sbx *docker.Sandbox, password string) {
	t.Helper()
	res, err := sbx.Exec(ctx, sandbox.ExecRequest{
		Argv: []string{"sh", "-c", "nohup /opt/mssql/bin/sqlservr >/tmp/seed-sqlservr.log 2>&1 &"},
		Env:  map[string]string{"ACCEPT_EULA": "Y", "MSSQL_SA_PASSWORD": password},
	})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("start seed engine: %+v, %v", res, err)
	}
}

func awaitReady(t *testing.T, ctx context.Context, sbx *docker.Sandbox, password string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		res, err := sbx.Exec(ctx, sandbox.ExecRequest{
			Argv: []string{"/opt/mssql-tools18/bin/sqlcmd", "-S", "127.0.0.1,1433", "-U", "sa",
				"-C", "-b", "-l", "2", "-h", "-1", "-Q", "SELECT 1"},
			Env:     map[string]string{"SQLCMDPASSWORD": password},
			Timeout: 10 * time.Second,
		})
		if err == nil && res.ExitCode == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("seed engine never became ready")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func mustSQL(t *testing.T, ctx context.Context, sbx *docker.Sandbox, password, stmt string) {
	t.Helper()
	res, err := sbx.Exec(ctx, sandbox.ExecRequest{
		Argv: []string{"/opt/mssql-tools18/bin/sqlcmd", "-S", "127.0.0.1,1433", "-U", "sa",
			"-C", "-b", "-Q", stmt},
		Env: map[string]string{"SQLCMDPASSWORD": password},
	})
	if err != nil {
		t.Fatalf("sql %q: %v", stmt, err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("sql %q: exit %d: %s%s", stmt, res.ExitCode, res.Stdout, res.Stderr)
	}
}

func destroy(t *testing.T, sbx *docker.Sandbox) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sbx.Destroy(ctx); err != nil {
		t.Errorf("destroy sandbox: %v", err)
	}
}

// TestBackupTypeSelectionEndToEnd reproduces issue #88 and proves the fix.
// A real SQL Server backup directory holds full, differential, and
// transaction log backups side by side, and the newest file is typically a
// log backup — which cannot create a database. Choosing by mtime alone
// therefore reports restore_failed on a perfectly restorable backup set: a
// false alarm, the direction that costs an operator's trust rather than
// merely withholding it.
func TestBackupTypeSelectionEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	provider := docker.New(nil)
	dir, rankDir := makeBackupSetFixture(t, ctx, provider)

	runner, err := adapter.New("mssql", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}

	t.Run("the newest file is a log backup that cannot create a database", func(t *testing.T) {
		assertNewestIsUnrestorable(t, ctx, provider, runner, dir)
	})
	t.Run("bak_dir restores the full backup instead", func(t *testing.T) {
		assertDirPicksTheFull(t, ctx, provider, runner, dir)
	})
	t.Run("a multi-set file restores its newest full set", func(t *testing.T) {
		assertMultiSetPicksNewestFull(t, ctx, provider, runner, dir)
	})
	t.Run("a directory with no full backup says so", func(t *testing.T) {
		assertNoFullBackupVerdict(t, ctx, provider, runner, dir)
	})
	t.Run("a stale backup copied in later does not outrank a newer one", func(t *testing.T) {
		assertRankingIgnoresFileTimes(t, ctx, provider, runner, rankDir)
	})
}

// assertRankingIgnoresFileTimes is issue #100 on a real engine: both files
// are full backups of the same database, the stale one carries the newest
// modification time because it was copied in afterwards, and the drill must
// restore the one the server finished last. The row counts differ, so which
// one was restored is a measurement.
func assertRankingIgnoresFileTimes(t *testing.T, ctx context.Context, provider *docker.Provider,
	runner *adapter.Runner, dir string) {
	now := time.Now()
	if err := os.Chtimes(filepath.Join(dir, "fresh.bak"), now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(dir, "stale.bak"), now, now); err != nil {
		t.Fatal(err)
	}

	sbx := freshSandbox(t, ctx, provider)
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "bak_dir", Path: dir},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if got := queryScalar(t, ctx, sbx, res.Connection.Database, "SELECT count(*) FROM dbo.orders"); got != strconv.Itoa(rankFreshRows) {
		t.Errorf("row count = %s, want %d — the drill restored the stale backup the copy made look fresh",
			got, rankFreshRows)
	}
}

// assertNewestIsUnrestorable is the premise of #88: pointed at the newest
// file directly, the drill fails — the file is a valid backup, just not one
// that can create a database.
func assertNewestIsUnrestorable(t *testing.T, ctx context.Context, provider *docker.Provider, runner *adapter.Runner, dir string) {
	sbx := freshSandbox(t, ctx, provider)
	_, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "bak", Path: filepath.Join(dir, "z-newest-log.trn")},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) {
		t.Fatalf("provision error = %v, want a refusal", err)
	}
	// And the refusal now teaches instead of quoting Msg 3118 at the
	// operator.
	if !strings.Contains(aerr.Message, "transaction log backup") {
		t.Errorf("message = %q, want it to name what the file actually is", aerr.Message)
	}
}

// assertDirPicksTheFull is the fix: the same directory whose newest file is
// a log backup now restores the full backup, and its data validates.
func assertDirPicksTheFull(t *testing.T, ctx context.Context, provider *docker.Provider, runner *adapter.Runner, dir string) {
	sbx := freshSandbox(t, ctx, provider)
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "bak_dir", Path: dir},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if rows := queryScalar(t, ctx, sbx, res.Connection.Database, "SELECT count(*) FROM dbo.orders"); rows != "1" {
		t.Errorf("rows = %s, want the full backup's single row", rows)
	}
	// The identity describes the artifact the engine chose, not the newest
	// file in the directory.
	if res.SourceIdentity.SizeBytes == 0 || !strings.HasPrefix(res.SourceIdentity.Checksum, "sha256:") {
		t.Errorf("source identity = %+v", res.SourceIdentity)
	}
	full, err := os.Stat(filepath.Join(dir, "a-full.bak"))
	if err != nil {
		t.Fatal(err)
	}
	if res.SourceIdentity.SizeBytes != full.Size() {
		t.Errorf("size_bytes = %d, want the chosen full backup's %d",
			res.SourceIdentity.SizeBytes, full.Size())
	}
}

// assertMultiSetPicksNewestFull covers appended media: one file holding
// full, log, full. Restoring without naming a set takes the first — the
// oldest backup on the file — so the newest full set is named explicitly.
func assertMultiSetPicksNewestFull(t *testing.T, ctx context.Context, provider *docker.Provider, runner *adapter.Runner, dir string) {
	sbx := freshSandbox(t, ctx, provider)
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "bak", Path: filepath.Join(dir, "multi", "multi.bak")},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	// The second full set was taken after a second row was inserted; the
	// first set has one row. Reading two proves the newest set was chosen.
	if rows := queryScalar(t, ctx, sbx, res.Connection.Database, "SELECT count(*) FROM dbo.orders"); rows != "2" {
		t.Errorf("rows = %s, want the newest full set's two rows", rows)
	}
}

// assertNoFullBackupVerdict proves the honest failure: a directory of log
// backups is not restorable, and the drill says exactly that instead of
// quoting the engine.
func assertNoFullBackupVerdict(t *testing.T, ctx context.Context, provider *docker.Provider, runner *adapter.Runner, dir string) {
	sbx := freshSandbox(t, ctx, provider)
	_, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "bak_dir", Path: filepath.Join(dir, "logsonly")},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) || aerr.Code != "source_not_found" {
		t.Fatalf("provision error = %v, want source_not_found", err)
	}
	for _, want := range []string{"no full backup", "transaction log backup", "SHA256SUMS"} {
		if !strings.Contains(aerr.Message, want) {
			t.Errorf("message = %q, want it to carry %q", aerr.Message, want)
		}
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

// queryScalar runs one query against the restored database as the sandbox
// superuser and returns its first line.
func queryScalar(t *testing.T, ctx context.Context, sbx *docker.Sandbox, database, query string) string {
	t.Helper()
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{
		Argv: []string{"/opt/mssql-tools18/bin/sqlcmd", "-S", "127.0.0.1,1433", "-U", "sa",
			"-C", "-b", "-l", "5", "-d", database, "-h", "-1", "-W", "-Q", "SET NOCOUNT ON; " + query},
		Env: map[string]string{"SQLCMDPASSWORD": "Probavi!DrillSandbox0"},
	})
	if err != nil || out.ExitCode != 0 {
		t.Fatalf("query %q: %v (exit %d, stderr %s)", query, err, out.ExitCode, out.Stderr)
	}
	return strings.TrimSpace(strings.SplitN(strings.TrimSpace(string(out.Stdout)), "\n", 2)[0])
}

// makeBackupSetFixture builds what a real SQL Server backup directory looks
// like: a full backup, a differential, and — newest of all — two
// transaction log backups, plus a checksum sidecar that is not backup media
// at all. It also produces a multi-set file (full, log, full appended into
// one) and a directory holding nothing but log backups.
func makeBackupSetFixture(t *testing.T, ctx context.Context, provider *docker.Provider) (dir, rankDir string) {
	t.Helper()
	seed, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)
	startEngine(t, ctx, seed, seedPassword)
	awaitReady(t, ctx, seed, seedPassword)

	mustSQL(t, ctx, seed, seedPassword, "CREATE DATABASE shop")
	mustSQL(t, ctx, seed, seedPassword, "ALTER DATABASE shop SET RECOVERY FULL")
	mustSQL(t, ctx, seed, seedPassword,
		"CREATE TABLE shop.dbo.orders (id INT PRIMARY KEY, total DECIMAL(10,2)); INSERT INTO shop.dbo.orders VALUES (1, 10.50)")

	// The order of these statements is the point: every artifact after the
	// full backup is newer than it.
	for _, stmt := range []string{
		"BACKUP DATABASE shop TO DISK = N'/tmp/a-full.bak'",
		"INSERT INTO shop.dbo.orders VALUES (2, 20.50)",
		"BACKUP DATABASE shop TO DISK = N'/tmp/m-diff.bak' WITH DIFFERENTIAL",
		"BACKUP LOG shop TO DISK = N'/tmp/y-log.trn'",
		"BACKUP LOG shop TO DISK = N'/tmp/z-newest-log.trn'",
		// Appended media: full, log, then a second full taken with both rows.
		"BACKUP DATABASE shop TO DISK = N'/tmp/multi.bak'",
		"BACKUP LOG shop TO DISK = N'/tmp/multi.bak'",
		"BACKUP DATABASE shop TO DISK = N'/tmp/multi.bak'",
		"BACKUP LOG shop TO DISK = N'/tmp/only-log.trn'",
		// One more full backup, later and with one more row, for the
		// ranking proof: /tmp/a-full.bak above is the stale one. The delay
		// keeps the two completion times apart in the header, which records
		// whole seconds.
		"INSERT INTO shop.dbo.orders VALUES (3, 30.50)",
		"WAITFOR DELAY '00:00:02'",
		"BACKUP DATABASE shop TO DISK = N'/tmp/rank-fresh.bak'",
	} {
		mustSQL(t, ctx, seed, seedPassword, stmt)
	}

	dir = t.TempDir()
	for _, name := range []string{"a-full.bak", "m-diff.bak", "y-log.trn", "z-newest-log.trn"} {
		copyFixture(t, ctx, seed, "/tmp/"+name, filepath.Join(dir, name))
	}
	// A checksum sidecar: not backup media, and probing it would look
	// exactly like a corrupt backup ("the volume ... is empty").
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte("abc123  a-full.bak\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Make the multi-set file and the log-only directory separate sources.
	for _, sub := range []string{"multi", "logsonly"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	copyFixture(t, ctx, seed, "/tmp/multi.bak", filepath.Join(dir, "multi", "multi.bak"))
	copyFixture(t, ctx, seed, "/tmp/only-log.trn", filepath.Join(dir, "logsonly", "only-log.trn"))
	if err := os.WriteFile(filepath.Join(dir, "logsonly", "SHA256SUMS"), []byte("abc123  only-log.trn\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// mtimes travel with docker cp, but the drill's rule must not depend on
	// that: stamp them explicitly so "newest" is a fact of this test.
	stamp := time.Now()
	for i, name := range []string{"a-full.bak", "m-diff.bak", "y-log.trn", "z-newest-log.trn"} {
		when := stamp.Add(time.Duration(i-10) * time.Minute)
		if err := os.Chtimes(filepath.Join(dir, name), when, when); err != nil {
			t.Fatal(err)
		}
	}

	// A directory of nothing but two full backups of the same database, for
	// the ranking proof: same type, same database, so only the completion
	// time each header records can tell them apart.
	rankDir = t.TempDir()
	copyFixture(t, ctx, seed, "/tmp/a-full.bak", filepath.Join(rankDir, "stale.bak"))
	copyFixture(t, ctx, seed, "/tmp/rank-fresh.bak", filepath.Join(rankDir, "fresh.bak"))
	return dir, rankDir
}

// rankFreshRows is how many rows the later of the two ranking fixtures
// carries; the earlier one has one.
const rankFreshRows = 3

func copyFixture(t *testing.T, ctx context.Context, sbx *docker.Sandbox, containerPath, dest string) {
	t.Helper()
	if out, err := exec.CommandContext(ctx, "docker", "cp", sbx.ID()+":"+containerPath, dest).CombinedOutput(); err != nil {
		t.Fatalf("extract fixture: %v: %s", err, out)
	}
}

// TestChainDrillEndToEnd reproduces issue #98 and proves the fix. A SQL
// Server backup set is a chain — a full backup, then differentials, then
// transaction log backups — and restoring only the full proves the state
// the database was in when that full was taken, not the state the backups
// can actually recover to. Both halves run against the same directory, so
// the difference is the drill's, not the fixture's.
func TestChainDrillEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	provider := docker.New(nil)
	dir := makeChainFixture(t, ctx, provider)

	runner, err := adapter.New("mssql", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}

	t.Run("bak_dir restores the full backup and stops there", func(t *testing.T) {
		sbx := freshSandbox(t, ctx, provider)
		res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
			Source:  adapter.ProvisionSource{Kind: "bak_dir", Path: dir},
			Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
		}, sbx)
		if err != nil {
			t.Fatalf("provision: %v", err)
		}
		if rows := queryScalar(t, ctx, sbx, res.Connection.Database, "SELECT count(*) FROM dbo.t"); rows != "1" {
			t.Errorf("rows = %s, want 1 — everything after the full backup is outside this drill", rows)
		}
	})

	t.Run("bak_chain replays the whole chain", func(t *testing.T) {
		sbx := freshSandbox(t, ctx, provider)
		res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
			Source:  adapter.ProvisionSource{Kind: "bak_chain", Path: dir},
			Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
		}, sbx)
		if err != nil {
			t.Fatalf("provision: %v", err)
		}
		// Seven rows: one before the full backup and six committed after
		// it, recovered through the differential and the logs.
		if rows := queryScalar(t, ctx, sbx, res.Connection.Database, "SELECT count(*) FROM dbo.t"); rows != "7" {
			t.Errorf("rows = %s, want 7 — the chain must recover what the logs carry", rows)
		}
		// The database is usable, which is what the final RECOVERY buys:
		// a chain left mid-restore answers nothing.
		health, err := runner.Healthcheck(ctx, &res.Connection, res.State, sbx)
		if err != nil || !health.Healthy {
			t.Errorf("healthcheck = %+v, %v — the chain must end recovered", health, err)
		}
		if res.SourceIdentity.SizeBytes == 0 || !strings.HasPrefix(res.SourceIdentity.Checksum, "sha256:") {
			t.Errorf("source identity = %+v", res.SourceIdentity)
		}
	})

	t.Run("a gap in the log sequence fails the drill", func(t *testing.T) {
		gapped := t.TempDir()
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			// Drop the log that bridges the differential to the last one.
			if e.Name() == "06-log.trn" {
				continue
			}
			copyFile(t, filepath.Join(dir, e.Name()), filepath.Join(gapped, e.Name()))
		}
		sbx := freshSandbox(t, ctx, provider)
		_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
			Source:  adapter.ProvisionSource{Kind: "bak_chain", Path: gapped},
			Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
		}, sbx)
		var aerr *adapter.Error
		if err == nil || !errors.As(err, &aerr) || aerr.Code != "source_not_found" {
			t.Fatalf("provision error = %v, want source_not_found from the chain builder", err)
		}
		if !strings.Contains(aerr.Message, "gap") {
			t.Errorf("message = %q, want the gap named", aerr.Message)
		}
	})
}

// makeChainFixture seeds a database in full recovery model and takes a
// real chain: a full backup, a log, a differential, a log, a second
// differential and two more logs — one row committed before each. A
// second database's full backup lands in the same directory, because a
// real backup directory holds more than one.
func makeChainFixture(t *testing.T, ctx context.Context, provider *docker.Provider) string {
	t.Helper()
	seed, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)
	startEngine(t, ctx, seed, seedPassword)
	awaitReady(t, ctx, seed, seedPassword)

	mustSQL(t, ctx, seed, seedPassword, "CREATE DATABASE shop")
	mustSQL(t, ctx, seed, seedPassword, "ALTER DATABASE shop SET RECOVERY FULL")
	mustSQL(t, ctx, seed, seedPassword, "CREATE TABLE shop.dbo.t (id INT PRIMARY KEY)")
	mustSQL(t, ctx, seed, seedPassword, "CREATE DATABASE other")

	// Each row is committed before the backup that must carry it, so a
	// chain that stops early shows up as a missing row rather than as a
	// timing coincidence.
	for _, step := range []struct {
		row    int
		backup string
	}{
		{1, "BACKUP DATABASE shop TO DISK = N'/tmp/01-full.bak'"},
		{2, "BACKUP LOG shop TO DISK = N'/tmp/02-log.trn'"},
		{3, "BACKUP DATABASE shop TO DISK = N'/tmp/03-diff.bak' WITH DIFFERENTIAL"},
		{4, "BACKUP LOG shop TO DISK = N'/tmp/04-log.trn'"},
		{5, "BACKUP DATABASE shop TO DISK = N'/tmp/05-diff.bak' WITH DIFFERENTIAL"},
		{6, "BACKUP LOG shop TO DISK = N'/tmp/06-log.trn'"},
		{7, "BACKUP LOG shop TO DISK = N'/tmp/07-log.trn'"},
	} {
		mustSQL(t, ctx, seed, seedPassword,
			fmt.Sprintf("INSERT INTO shop.dbo.t VALUES (%d)", step.row))
		mustSQL(t, ctx, seed, seedPassword, step.backup)
	}
	mustSQL(t, ctx, seed, seedPassword, "BACKUP DATABASE other TO DISK = N'/tmp/99-other.bak'")

	dir := t.TempDir()
	for _, name := range []string{"01-full.bak", "02-log.trn", "03-diff.bak", "04-log.trn",
		"05-diff.bak", "06-log.trn", "07-log.trn"} {
		copyFixture(t, ctx, seed, "/tmp/"+name, filepath.Join(dir, name))
	}
	return dir
}

// TestTheEngineDisablesTemporalRetentionOnRestore closes this engine's
// line of issue #166.
//
// SQL Server's per-row expiry is a system-versioned temporal table with a
// HISTORY_RETENTION_PERIOD, cleaned by a background task. Measured on both
// verified versions, the engine turns that task **off by itself** when a
// database is restored — `is_temporal_history_retention_enabled` goes from
// 1 on the source to 0 in the drill — while the table keeps declaring the
// operator's own period. So the sibling adapters' pin has nothing to do
// here.
//
// That is an upstream behaviour this adapter depends on without stating
// it, which is exactly the shape that is worth a test: the day a release
// stops doing it, this is what notices.
//
// The Agent is the other half of the question, and it answers itself: its
// jobs live in msdb, and a drill restores one user database.
func TestTheEngineDisablesTemporalRetentionOnRestore(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	provider := docker.New(nil)

	fixture := filepath.Join(t.TempDir(), "temporal.bak")
	makeTemporalFixture(t, ctx, provider, fixture)

	sbx, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("mssql", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "bak", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	restored := res.Connection.Database

	if got := drillQuery(t, ctx, sbx, "master",
		"SELECT is_temporal_history_retention_enabled FROM sys.databases WHERE name='"+restored+"'"); got != "0" {
		t.Errorf("temporal history retention = %q in the drill, want it off — the engine used to "+
			"disable it on restore, and the drill relies on that", got)
	}
	// What the backup declared is still what a check reads.
	if got := drillQuery(t, ctx, sbx, restored,
		"SELECT CONVERT(varchar,history_retention_period) FROM sys.tables WHERE name='orders'"); got != "1" {
		t.Errorf("declared retention period = %q, want the backup's own 1 DAY", got)
	}
	// And the history the artifact carried is all there.
	if got := drillQuery(t, ctx, sbx, restored, "SELECT COUNT(*) FROM dbo.orders_history"); got != "50" {
		t.Errorf("history rows = %q, want 50", got)
	}
	if got := drillQuery(t, ctx, sbx, restored, "SELECT COUNT(*) FROM dbo.orders"); got != "50" {
		t.Errorf("current rows = %q, want 50", got)
	}
	// The Agent's jobs live in msdb, which a user-database drill never
	// restores — so no schedule of the operator's can run here.
	if got := drillQuery(t, ctx, sbx, "msdb", "SELECT COUNT(*) FROM dbo.sysjobs"); got != "0" {
		t.Errorf("agent jobs in the drill = %q, want none", got)
	}
}

// makeTemporalFixture backs up a database whose temporal history is older
// than the retention period it declares — the shape a cleanup task would
// act on if one were running.
func makeTemporalFixture(t *testing.T, ctx context.Context, provider *docker.Provider, dest string) {
	t.Helper()
	seed, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)

	startEngine(t, ctx, seed, seedPassword)
	awaitReady(t, ctx, seed, seedPassword)
	const versioningOn = "ALTER TABLE dbo.orders SET (SYSTEM_VERSIONING = ON " +
		"(HISTORY_TABLE = dbo.orders_history, HISTORY_RETENTION_PERIOD = 1 DAY))"
	for _, stmt := range []string{
		"CREATE DATABASE shop",
		"USE shop; CREATE TABLE dbo.orders (id INT PRIMARY KEY, total INT, " +
			"SysStart DATETIME2 GENERATED ALWAYS AS ROW START NOT NULL, " +
			"SysEnd DATETIME2 GENERATED ALWAYS AS ROW END NOT NULL, " +
			"PERIOD FOR SYSTEM_TIME (SysStart, SysEnd)) WITH (SYSTEM_VERSIONING = ON " +
			"(HISTORY_TABLE = dbo.orders_history, HISTORY_RETENTION_PERIOD = 1 DAY))",
		"USE shop; INSERT INTO dbo.orders (id,total) SELECT TOP 50 " +
			"ROW_NUMBER() OVER (ORDER BY (SELECT NULL)), 10 FROM sys.all_objects",
		// History rows are written by the engine, so backdating them means
		// turning versioning off, inserting, and turning it back on — each
		// its own batch, because the metadata change is not visible inside
		// the one that made it.
		"USE shop; ALTER TABLE dbo.orders SET (SYSTEM_VERSIONING = OFF)",
		"USE shop; INSERT INTO dbo.orders_history (id,total,SysStart,SysEnd) SELECT id, 1, " +
			"DATEADD(day,-30,SYSUTCDATETIME()), DATEADD(day,-20,SYSUTCDATETIME()) FROM dbo.orders",
		"USE shop; " + versioningOn,
		"BACKUP DATABASE shop TO DISK = N'/tmp/temporal.bak' WITH INIT, FORMAT",
	} {
		mustSQL(t, ctx, seed, seedPassword, stmt)
	}

	if out, err := exec.CommandContext(ctx, "docker", "cp",
		seed.ID()+":/tmp/temporal.bak", dest).CombinedOutput(); err != nil {
		t.Fatalf("extract fixture: %v: %s", err, out)
	}
}

// drillQuery asks the restored engine one scalar question.
func drillQuery(t *testing.T, ctx context.Context, sbx *docker.Sandbox, database, query string) string {
	t.Helper()
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{
		Argv: []string{"/opt/mssql-tools18/bin/sqlcmd", "-S", "127.0.0.1,1433", "-U", "sa",
			"-C", "-b", "-l", "5", "-d", database, "-h", "-1", "-W", "-Q", "SET NOCOUNT ON; " + query},
		Env: map[string]string{"SQLCMDPASSWORD": "Probavi!DrillSandbox0"},
	})
	if err != nil {
		t.Fatalf("drill query %q: %v", query, err)
	}
	if out.ExitCode != 0 {
		t.Fatalf("drill query %q: exit %d: %s%s", query, out.ExitCode, out.Stdout, out.Stderr)
	}
	return strings.TrimSpace(string(out.Stdout))
}
