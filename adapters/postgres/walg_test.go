package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// walg_test.go builds wal-g repositories in the shape a real one has,
// measured against wal-g v3.0.9 and PostgreSQL 16: basebackups_005 holding
// base_<name>/ beside base_<name>_backup_stop_sentinel.json, and wal_005
// holding the compressed segments.

const walgPG16 = 160015

// walgSpec describes a repository to write out for one test.
type walgSpec struct {
	backups  []walgBackup
	segments []string
	noWAL    bool
	noBase   bool
	// strays are files written into basebackups_005 that are not
	// sentinels, and one sentinel that is not JSON.
	strays bool
}

// twoWalgBackups is the ordinary shape: an older base backup and a newer
// one, each with its own sentinel.
func twoWalgBackups() []walgBackup {
	return []walgBackup{
		{
			name:       "base_000000010000000000000002",
			finishTime: time.Date(2026, 9, 24, 2, 0, 0, 0, time.UTC),
			pgVersion:  walgPG16,
		},
		{
			name:       "base_000000010000000000000007",
			finishTime: time.Date(2026, 9, 25, 2, 0, 0, 0, time.UTC),
			pgVersion:  walgPG16,
		},
	}
}

// writeWalgRepo materialises a repository and returns its path.
func writeWalgRepo(t *testing.T, spec walgSpec) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "walg")
	write := func(rel, content string) {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if !spec.noBase {
		if err := os.MkdirAll(filepath.Join(root, walgBaseDir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, b := range spec.backups {
		write(walgBaseDir+"/"+b.name+"/tar_partitions/part_001.tar.lz4", "backup bytes")
		write(walgBaseDir+"/"+b.name+walgSentinelSuffix, fmt.Sprintf(
			`{"LSN":33554472,"PgVersion":%d,"FinishLSN":33554688,"SystemIdentifier":768942371,`+
				`"UncompressedSize":23423472,"CompressedSize":4358706,"Version":2,`+
				`"StartTime":%q,"FinishTime":%q,"Hostname":"seed","DataDir":"/var/lib/postgresql/data",`+
				`"IsPermanent":false}`,
			b.pgVersion,
			b.finishTime.Add(-time.Second).Format(time.RFC3339Nano),
			b.finishTime.Format(time.RFC3339Nano)))
	}
	if spec.strays {
		// A half-written sentinel — wal-g writes it last, so this is a
		// backup still in progress — and a file that is not one at all.
		write(walgBaseDir+"/base_000000010000000000000009"+walgSentinelSuffix, `{"FinishTime":`)
		write(walgBaseDir+"/README", "not a sentinel\n")
	}
	if !spec.noWAL {
		if err := os.MkdirAll(filepath.Join(root, walgWALDir), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, seg := range spec.segments {
			write(walgWALDir+"/"+seg+".lz4", "wal bytes")
		}
	}
	return root
}

// TestWalgSeries pins the conversion the version pre-check rests on.
// server_version_num changed shape at 10, and a 9.x backup still has to be
// refused against the right sandbox.
func TestWalgSeries(t *testing.T) {
	for _, tc := range []struct {
		num  int
		want string
	}{
		{160015, "16"},
		{100000, "10"},
		{180001, "18"},
		{90624, "9.6"},
		{90523, "9.5"},
		{0, ""},
		{-1, ""},
	} {
		if got := walgSeries(tc.num); got != tc.want {
			t.Errorf("walgSeries(%d) = %q, want %q", tc.num, got, tc.want)
		}
	}
}

func TestReadWalgBackups(t *testing.T) {
	repo := writeWalgRepo(t, walgSpec{backups: twoWalgBackups(), strays: true})
	got := readWalgBackups(repo)
	if len(got) != 2 {
		t.Fatalf("read %d backups, want 2 — a half-written sentinel is a backup in progress, "+
			"not a catalogue entry, and a README is not one at all: %+v", len(got), got)
	}
	// Sorted oldest first, so the caller can name the oldest in a refusal.
	if !got[0].finishTime.Before(got[1].finishTime) {
		t.Errorf("backups = %+v, want them ordered oldest first", got)
	}
	if got[1].pgVersion != walgPG16 {
		t.Errorf("pgVersion = %d, want the sentinel's own %d", got[1].pgVersion, walgPG16)
	}
}

func TestSelectWalgBackup(t *testing.T) {
	backups := twoWalgBackups()

	t.Run("no target takes the newest", func(t *testing.T) {
		chosen, perr := selectWalgBackup(backups, time.Time{})
		if perr != nil {
			t.Fatalf("selectWalgBackup: %+v", perr)
		}
		if chosen.name != backups[1].name {
			t.Errorf("chose %s, want the newest %s", chosen.name, backups[1].name)
		}
	})

	// Recovery only rolls forward, so a target takes the newest backup
	// that finished at or before it — never a later one to roll back from.
	t.Run("a target takes the newest backup before it", func(t *testing.T) {
		target := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
		chosen, perr := selectWalgBackup(backups, target)
		if perr != nil {
			t.Fatalf("selectWalgBackup: %+v", perr)
		}
		if chosen.name != backups[0].name {
			t.Errorf("chose %s, want the older %s — the newer one finished after the target",
				chosen.name, backups[0].name)
		}
	})

	t.Run("a target before every backup is refused, naming the oldest", func(t *testing.T) {
		target := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
		_, perr := selectWalgBackup(backups, target)
		if perr == nil || perr.Code != "source_not_found" ||
			!strings.Contains(perr.Message, "only rolls forward") ||
			!strings.Contains(perr.Message, "2026-09-24T02:00:00Z") {
			t.Fatalf("perr = %+v, want the oldest backup's own instant in the refusal", perr)
		}
	})

	t.Run("an empty catalogue is corrupt, not empty", func(t *testing.T) {
		_, perr := selectWalgBackup(nil, time.Time{})
		if perr == nil || perr.Code != "source_corrupt" {
			t.Fatalf("perr = %+v, want source_corrupt", perr)
		}
	})
}

func TestResolveWalg(t *testing.T) {
	t.Run("a whole repository", func(t *testing.T) {
		repo := writeWalgRepo(t, walgSpec{
			backups:  twoWalgBackups(),
			segments: []string{"000000010000000000000007", "000000010000000000000008"},
		})
		src, perr := resolveWalg(repo)
		if perr != nil {
			t.Fatalf("resolveWalg: %+v", perr)
		}
		if !strings.HasPrefix(src.checksum, "sha256:") || src.sizeBytes == 0 {
			t.Errorf("identity = %+v, want a measured tree", src)
		}
		// Dated by the newest backup, which is the one a drill without a
		// target restores.
		if src.createdAt == nil || *src.createdAt != "2026-09-25T02:00:00.000Z" {
			t.Errorf("created_at = %v, want the newest backup's own finish time", src.createdAt)
		}
	})

}

// TestResolveWalgRefusals covers the shapes a wal-g prefix can have and
// still not be one a drill can prove.
func TestResolveWalgRefusals(t *testing.T) {
	// A prefix with backups and no archive restores to the instant of a
	// base backup and cannot recover past it — which is not this kind.
	t.Run("refusals", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			spec walgSpec
			want string
		}{
			{"no wal archive", walgSpec{backups: twoWalgBackups(), noWAL: true}, walgWALDir},
			{"no backups directory", walgSpec{noBase: true, segments: []string{"x"}}, walgBaseDir},
		} {
			t.Run(tc.name, func(t *testing.T) {
				_, perr := resolveWalg(writeWalgRepo(t, tc.spec))
				if perr == nil || perr.Code != "invalid_request" ||
					!strings.Contains(perr.Message, tc.want) {
					t.Fatalf("perr = %+v, want the missing %s named", perr, tc.want)
				}
			})
		}
	})

	t.Run("a path that is not there", func(t *testing.T) {
		_, perr := resolveWalg(filepath.Join(t.TempDir(), "gone"))
		if perr == nil || perr.Code != "source_not_found" {
			t.Fatalf("perr = %+v, want source_not_found", perr)
		}
	})

	t.Run("a file rather than a directory", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "repo")
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, perr := resolveWalg(path)
		if perr == nil || perr.Code != "invalid_request" {
			t.Fatalf("perr = %+v, want invalid_request", perr)
		}
	})
}

// TestWalgRestoreScript pins the two things the script cannot get wrong:
// the repository reaches both the fetch and the restore_command, and
// recovery promotes rather than pausing.
func TestWalgRestoreScript(t *testing.T) {
	const repo, pgdata = "/scratch/probavi-walg-repo", "/var/lib/postgresql/data"
	target := time.Date(2026, 9, 25, 14, 30, 0, 0, time.UTC)

	script := walgRestoreScript(repo, pgdata, "base_0001", true, target)
	if n := strings.Count(script, walgFilePrefixEnv+"="+repo); n != 2 {
		t.Errorf("the repository appears %d times, want 2 — restore_command runs in PostgreSQL's "+
			"own shell and inherits nothing from this one", n)
	}
	if !strings.Contains(script, "recovery_target_action = 'promote'") {
		t.Error("recovery does not promote; the default pauses and a drill would hang at the target")
	}
	if !strings.Contains(script, "recovery_target_time = '2026-09-25 14:30:00.000000+00'") {
		t.Errorf("the target is missing or not in the form PostgreSQL parses:\n%s", script)
	}
	if !strings.Contains(script, "backup-fetch "+pgdata+" base_0001") {
		t.Error("the fetch does not name the backup the selection chose")
	}
	if !strings.Contains(script, "archive_mode = off") {
		t.Error("archive_mode is not disabled; a restored sandbox must not archive anywhere")
	}

	// Without a target there is no recovery_target_time at all: recovery
	// runs to the end of the archive.
	plain := walgRestoreScript(repo, pgdata, "base_0001", false, time.Time{})
	if strings.Contains(plain, "recovery_target_time") {
		t.Error("a drill with no target must not pin one")
	}
}

// walgPayload drives the kind through the whole provision flow.
func walgPayload(repo, pitr string) string {
	target := ""
	if pitr != "" {
		target = fmt.Sprintf(`,"pitr":{"target_time":%q}`, pitr)
	}
	return fmt.Sprintf(
		`{"source":{"kind":"walg","path":%q,"params":{}},"sandbox":{"scratch_dir":"/scratch"},`+
			`"options":{}%s}`, repo, target)
}

// walgHandler simulates the idle sandbox through the wal-g flow, recording
// a label per call so the test can assert the order as well as the calls.
func walgHandler(t *testing.T, sequence *[]string, script *string) func(verbCall) (any, *protoError) {
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			*sequence = append(*sequence, "put_file")
			return putFileValue{BytesCopied: 42, DurationSeconds: 0.5}, nil
		}
		args := execArgs{}
		if err := json.Unmarshal(call.Args, &args); err != nil {
			t.Fatalf("exec args: %v", err)
		}
		head := strings.Join(args.Argv[:min(3, len(args.Argv))], " ")
		switch {
		case args.Argv[0] == "sh" && strings.Contains(args.Argv[2], "PGDATA"):
			*sequence = append(*sequence, "resolve PGDATA")
			return outExec("/var/lib/postgresql/18/docker"), nil
		case args.Argv[0] == "sh" && strings.Contains(args.Argv[2], "backup-fetch"):
			*sequence = append(*sequence, "fetch")
			*script = args.Argv[2]
			return execValue{ExitCode: 0, DurationSeconds: 3.5}, nil
		case args.Argv[0] == "sh" && strings.Contains(args.Argv[2], "pg_ctl"):
			*sequence = append(*sequence, "pg_ctl start")
			return execValue{ExitCode: 0, DurationSeconds: 2.0}, nil
		case args.Argv[0] == "wal-g":
			*sequence = append(*sequence, "wal-g version")
			return okExec(0), nil
		case args.Argv[0] == "pg_isready" && len(*sequence) <= 1:
			return okExec(2), nil // idle: engine not running
		case args.Argv[0] == "pg_isready":
			return okExec(0), nil // after start: ready
		case args.Argv[0] == "psql":
			return outExec("f\n"), nil // recovery finished, promoted
		case args.Argv[0] == "postgres":
			return outExec("postgres (PostgreSQL) 16.15\n"), nil
		default:
			*sequence = append(*sequence, head)
			return okExec(0), nil
		}
	}
}

func walgProvisionFixture(t *testing.T) string {
	t.Helper()
	return writeWalgRepo(t, walgSpec{
		backups:  twoWalgBackups(),
		segments: []string{"000000010000000000000007", "000000010000000000000008"},
	})
}

// TestProvisionWalgHappyPath pins the order the flow cannot get wrong: the
// pairings that can never work are refused before the repository moves,
// and the repository is handed to postgres before anything reads it.
func TestProvisionWalgHappyPath(t *testing.T) {
	var sequence []string
	var script string
	line, _, exit := driveOp(t, "provision", walgPayload(walgProvisionFixture(t), ""),
		walgHandler(t, &sequence, &script))
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	f := parseFinal(t, line)
	if !f.OK {
		t.Fatalf("final = %+v", f)
	}

	joined := strings.Join(sequence, " | ")
	for _, want := range []string{"wal-g version", "put_file", "resolve PGDATA", "fetch", "pg_ctl start"} {
		if !strings.Contains(joined, want) {
			t.Errorf("sequence %q missing %q", joined, want)
		}
	}
	if strings.Index(joined, "put_file") < strings.Index(joined, "wal-g version") {
		t.Error("the image check must run before the repository is transferred")
	}
	// The staged repository is handed over before wal-g or PostgreSQL read
	// it: put_file lands it owned by the identity the sandbox execs as.
	if !strings.Contains(script, "chown -R postgres:postgres /scratch/probavi-walg-repo") {
		t.Errorf("the restore script does not hand the repository to postgres:\n%s", script)
	}
	if !strings.Contains(script, "backup-fetch /var/lib/postgresql/18/docker base_000000010000000000000007") {
		t.Errorf("the fetch does not use the resolved PGDATA and the newest backup:\n%s", script)
	}

	res := provisionWire{}
	if err := json.Unmarshal(f.Payload, &res); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if res.Timings.Restore != 3.5 || res.Timings.Transfer != 0.5 || res.Timings.EngineReady <= 0 {
		t.Errorf("timings = %+v", res.Timings)
	}
	if res.State["mode"] != "walg" || res.State["backup_id"] != "base_000000010000000000000007" {
		t.Errorf("state = %+v, want the backup the selection chose named in the record", res.State)
	}
}

// TestProvisionWalgPITR: a target picks the older backup and pins
// recovery_target_time, so the drill proves the instant it asked for.
func TestProvisionWalgPITR(t *testing.T) {
	var sequence []string
	var script string
	line, _, _ := driveOp(t, "provision",
		walgPayload(walgProvisionFixture(t), "2026-09-24T12:00:00Z"),
		walgHandler(t, &sequence, &script))
	if f := parseFinal(t, line); !f.OK {
		t.Fatalf("final = %+v", f)
	}
	if !strings.Contains(script, "base_000000010000000000000002") {
		t.Errorf("the fetch did not take the backup that finished before the target:\n%s", script)
	}
	if !strings.Contains(script, "recovery_target_time = '2026-09-24 12:00:00.000000+00'") {
		t.Errorf("recovery does not stop at the requested instant:\n%s", script)
	}
}

// TestProvisionWalgRefusals covers what fails the drill before a byte
// moves, each in its own words.
func TestProvisionWalgRefusals(t *testing.T) {
	repo := walgProvisionFixture(t)
	for _, tc := range []struct {
		name    string
		payload string
		handler func(verbCall) (any, *protoError)
		code    string
		want    string
	}{
		{
			"an image without wal-g", walgPayload(repo, ""),
			func(call verbCall) (any, *protoError) {
				args := execArgs{}
				if err := json.Unmarshal(call.Args, &args); err != nil {
					t.Fatal(err)
				}
				if args.Argv[0] == "wal-g" {
					return errExec(127, "wal-g: not found"), nil
				}
				if args.Argv[0] == "postgres" {
					return outExec("postgres (PostgreSQL) 16.15\n"), nil
				}
				return okExec(2), nil // idle
			},
			"invalid_request", "lacks wal-g",
		},
		{
			"a target older than every backup", walgPayload(repo, "2026-09-01T00:00:00Z"),
			mustNotBeCalled(t), "source_not_found", "only rolls forward",
		},
		{
			"a repository with no archive",
			walgPayload(writeWalgRepo(t, walgSpec{backups: twoWalgBackups(), noWAL: true}), ""),
			mustNotBeCalled(t), "invalid_request", walgWALDir,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line, _, _ := driveOp(t, "provision", tc.payload, tc.handler)
			f := parseFinal(t, line)
			if f.OK || f.Error.Code != tc.code || !strings.Contains(f.Error.Message, tc.want) {
				t.Fatalf("final = %+v, want %s carrying %q", f, tc.code, tc.want)
			}
		})
	}
}

// TestWalgRefusesAMajorVersionMismatch: the sentinel states the server
// that took the backup, so the one pairing a physical restore can never
// survive is refused before the repository moves.
func TestWalgRefusesAMajorVersionMismatch(t *testing.T) {
	backups := twoWalgBackups()
	for i := range backups {
		backups[i].pgVersion = 140012 // a PostgreSQL 14 repository
	}
	repo := writeWalgRepo(t, walgSpec{backups: backups, segments: []string{"000000010000000000000007"}})
	var sequence []string
	var script string
	line, _, _ := driveOp(t, "provision", walgPayload(repo, ""), walgHandler(t, &sequence, &script))
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "invalid_request" ||
		!strings.Contains(f.Error.Message, "PostgreSQL 14") {
		t.Fatalf("final = %+v, want the major-version pairing refused by name", f)
	}
	if strings.Contains(strings.Join(sequence, " "), "put_file") {
		t.Error("the repository moved anyway — the check exists to run before it")
	}
}

// TestProvisionWalgFetchFailure: a fetch that fails is a verdict about the
// repository, not an infrastructure error, and it says which step failed
// rather than leaving a bare exit status.
func TestProvisionWalgFetchFailure(t *testing.T) {
	handler := func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			return putFileValue{BytesCopied: 42, DurationSeconds: 0.5}, nil
		}
		args := execArgs{}
		if err := json.Unmarshal(call.Args, &args); err != nil {
			t.Fatal(err)
		}
		switch {
		case args.Argv[0] == "sh" && strings.Contains(args.Argv[2], "PGDATA"):
			return outExec("/var/lib/postgresql/18/docker"), nil
		case args.Argv[0] == "sh" && strings.Contains(args.Argv[2], "backup-fetch"):
			return errExec(1, "ERROR: archive 'base_x' does not exist"), nil
		case args.Argv[0] == "wal-g":
			return okExec(0), nil
		case args.Argv[0] == "postgres":
			return outExec("postgres (PostgreSQL) 16.15\n"), nil
		default:
			return okExec(2), nil // idle
		}
	}
	line, _, _ := driveOp(t, "provision", walgPayload(walgProvisionFixture(t), ""), handler)
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "restore_failed" ||
		!strings.Contains(f.Error.Message, walgRestoreFailHint) ||
		!strings.Contains(f.Error.Message, "does not exist") {
		t.Fatalf("final = %+v, want restore_failed naming the step and the tool's own words", f)
	}
}

// TestWalgCreatedAtWithoutASentinel: a prefix whose backups cannot be read
// dates to nothing rather than to a guess. resolveWalg refuses such a
// repository for other reasons, so this pins the dating rule on its own.
func TestWalgCreatedAtWithoutASentinel(t *testing.T) {
	repo := writeWalgRepo(t, walgSpec{segments: []string{"000000010000000000000007"}})
	if got := walgCreatedAt(repo); got != nil {
		t.Errorf("created_at = %q, want nil — nothing in the repository states an instant", *got)
	}
	if got := walgCreatedAt(filepath.Join(t.TempDir(), "gone")); got != nil {
		t.Errorf("created_at = %q for a path that is not there, want nil", *got)
	}
}
