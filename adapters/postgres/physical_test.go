package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func writeRepoFixture(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	for path, content := range map[string]string{
		"backup/demo/backup.info": "backup-info",
		"archive/demo/wal-001":    "wal-bytes",
	} {
		full := filepath.Join(repo, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	return repo
}

func TestDirChecksum(t *testing.T) {
	repo := writeRepoFixture(t)
	sum1, size, perr := dirChecksum(repo)
	if perr != nil {
		t.Fatalf("dirChecksum: %+v", perr)
	}
	if size != int64(len("backup-info")+len("wal-bytes")) {
		t.Errorf("size=%d", size)
	}
	sum2, _, _ := dirChecksum(repo)
	if sum1 != sum2 {
		t.Error("dirChecksum is not deterministic")
	}
	if err := os.WriteFile(filepath.Join(repo, "archive/demo/wal-001"), []byte("tampered!"), 0o600); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if sum3, _, _ := dirChecksum(repo); sum3 == sum1 {
		t.Error("content change must change the tree hash")
	}

	if _, _, perr := dirChecksum(t.TempDir()); perr == nil || perr.Code != "source_not_found" {
		t.Errorf("empty dir: %+v, want source_not_found", perr)
	}
}

func TestResolveRepo(t *testing.T) {
	repo := writeRepoFixture(t)
	src, perr := resolveSource(context.Background(), "pgbackrest", repo, nil)
	if perr != nil {
		t.Fatalf("resolveSource: %+v", perr)
	}
	if !strings.HasPrefix(src.checksum, "sha256:") || src.createdAt != nil {
		t.Errorf("src = %+v — a repository's mtimes date copies, not backups", src)
	}

	file := filepath.Join(t.TempDir(), "f.dump")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, perr := resolveSource(context.Background(), "pgbackrest", file, nil); perr == nil || perr.Code != "invalid_request" {
		t.Errorf("file as repo: %+v, want invalid_request", perr)
	}
	if _, perr := resolveSource(context.Background(), "pgbackrest", filepath.Join(t.TempDir(), "gone"), nil); perr == nil || perr.Code != "source_not_found" {
		t.Errorf("missing repo: %+v, want source_not_found", perr)
	}
}

func pgbackrestPayload(repo, stanza string) string {
	params := "{}"
	if stanza != "" {
		params = fmt.Sprintf(`{"stanza":%q}`, stanza)
	}
	return fmt.Sprintf(`{"source":{"kind":"pgbackrest","path":%q,"params":%s},"sandbox":{"scratch_dir":"/scratch"},"options":{}}`, repo, params)
}

// physicalHandler simulates the idle sandbox through the whole physical
// flow, recording the argv sequence.
func physicalHandler(t *testing.T, sequence *[]string) func(verbCall) (any, *protoError) {
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
		if args.Argv[0] == "sh" && strings.Contains(args.Argv[2], "pg_ctl") {
			head = "sh: pg_ctl start"
		}
		if args.Argv[0] == "sh" && strings.Contains(args.Argv[2], "PGDATA") {
			head = "sh: resolve PGDATA"
		}
		*sequence = append(*sequence, head)
		switch {
		case head == "sh: resolve PGDATA":
			// What postgres:18 answers; the adapter must follow it rather
			// than the path every image before 18 used.
			return outExec("/var/lib/postgresql/18/docker"), nil
		case args.Argv[0] == "pg_isready" && len(*sequence) <= 2:
			return okExec(2), nil // idle: engine not running
		case args.Argv[0] == "pg_isready":
			return okExec(0), nil // after start: ready
		case args.Argv[0] == "psql":
			return outExec("f\n"), nil // recovery finished, promoted
		case head == "gosu postgres pgbackrest":
			return execValue{ExitCode: 0, DurationSeconds: 3.5}, nil
		case head == "sh: pg_ctl start":
			return execValue{ExitCode: 0, DurationSeconds: 2.0}, nil
		default:
			return okExec(0), nil
		}
	}
}

func TestProvisionPhysicalHappyPath(t *testing.T) {
	repo := writeRepoFixture(t)
	var sequence []string
	line, _, exit := driveOp(t, "provision", pgbackrestPayload(repo, "demo"), physicalHandler(t, &sequence))
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	f := parseFinal(t, line)
	if !f.OK {
		t.Fatalf("final = %+v", f)
	}

	joined := strings.Join(sequence, " | ")
	for _, want := range []string{
		"pg_isready",               // idle check first
		"pgbackrest version",       // capability check
		"put_file",                 // repo transfer
		"sh -c",                    // prepare (config + empty PGDATA + chown)
		"gosu postgres pgbackrest", // the restore itself
		"sh: pg_ctl start",         // start + recovery
		"psql -h 127.0.0.1",        // promotion check: recovery must be over
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("sequence %q missing %q", joined, want)
		}
	}
	if strings.Index(joined, "put_file") < strings.Index(joined, "pgbackrest version") {
		t.Error("capability checks must run before the repo transfer")
	}

	res := provisionWire{}
	if err := json.Unmarshal(f.Payload, &res); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if res.Timings.Restore != 3.5 || res.Timings.Transfer != 0.5 || res.Timings.EngineReady <= 0 {
		t.Errorf("timings = %+v", res.Timings)
	}
	if res.State["mode"] != "physical" || res.State["stanza"] != "demo" {
		t.Errorf("state = %+v", res.State)
	}
}

func mustNotBeCalled(t *testing.T) func(verbCall) (any, *protoError) {
	return func(verbCall) (any, *protoError) {
		t.Error("no sandbox call may happen for this case")
		return nil, protoErr("internal", false, "must not be called")
	}
}

// alreadyRunning simulates pg_isready reporting a live engine.
// sandboxPGData is what postgres:18 answers when the adapter asks where
// the cluster belongs. Every handler that reaches the restore has to give
// it, because the adapter refuses a directory it cannot read out of the
// sandbox rather than falling back to the path 17 and earlier used.
const sandboxPGData = "/var/lib/postgresql/18/docker"

// isPGDataProbe reports whether this exec is the adapter asking.
func isPGDataProbe(args execArgs) bool {
	return len(args.Argv) > 2 && args.Argv[0] == "sh" && strings.Contains(args.Argv[2], "PGDATA")
}

func alreadyRunning(verbCall) (any, *protoError) { return okExec(0), nil }

// noPgbackrest simulates an idle sandbox whose image lacks pgbackrest.
func noPgbackrest() func(verbCall) (any, *protoError) {
	call := 0
	return func(verbCall) (any, *protoError) {
		call++
		if call == 1 {
			return okExec(2), nil // idle
		}
		return errExec(127, "pgbackrest: not found"), nil
	}
}

// restoreFails simulates the flow up to a failing pgbackrest restore.
func restoreFails(t *testing.T) func(verbCall) (any, *protoError) {
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			return putFileValue{}, nil
		}
		args := execArgs{}
		if err := json.Unmarshal(call.Args, &args); err != nil {
			t.Fatalf("args: %v", err)
		}
		switch {
		case isPGDataProbe(args):
			return outExec(sandboxPGData), nil
		case args.Argv[0] == "pg_isready":
			return okExec(2), nil
		case len(args.Argv) > 2 && args.Argv[2] == "pgbackrest":
			return errExec(1, "ERROR: [055]: unable to load info file"), nil
		default:
			return okExec(0), nil
		}
	}
}

func TestProvisionPhysicalFailures(t *testing.T) {
	repo := writeRepoFixture(t)
	tests := []struct {
		name       string
		stanza     string
		handler    func(verbCall) (any, *protoError)
		wantCode   string
		wantSubstr string
	}{
		{"missing stanza", "", mustNotBeCalled(t), "invalid_request", "stanza"},
		{"stanza injection rejected", "demo; rm -rf /", mustNotBeCalled(t), "invalid_request", "stanza"},
		{"running engine refused", "demo", alreadyRunning, "invalid_request", "idle sandbox"},
		{"image without pgbackrest refused", "demo", noPgbackrest(), "invalid_request", "lacks pgbackrest"},
		{"restore failure classified", "demo", restoreFails(t), "restore_failed", "unable to load info file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line, _, _ := driveOp(t, "provision", pgbackrestPayload(repo, tt.stanza), tt.handler)
			f := parseFinal(t, line)
			if f.OK || f.Error.Code != tt.wantCode || !strings.Contains(f.Error.Message, tt.wantSubstr) {
				t.Errorf("final = %+v, want %s containing %q", f, tt.wantCode, tt.wantSubstr)
			}
		})
	}
}

func pitrPayload(repo, target string) string {
	return fmt.Sprintf(
		`{"source":{"kind":"pgbackrest","path":%q,"params":{"stanza":"demo"}},"sandbox":{"scratch_dir":"/scratch"},"options":{},"pitr":{"target_time":%q}}`,
		repo, target)
}

func TestProvisionPhysicalPITR(t *testing.T) {
	repo := writeRepoFixture(t)
	var sequence []string
	line, calls, exit := driveOp(t, "provision", pitrPayload(repo, "2026-07-30T14:32:00Z"), physicalHandler(t, &sequence))
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	if f := parseFinal(t, line); !f.OK {
		t.Fatalf("final = %+v", f)
	}

	var restoreArgv []string
	for _, call := range calls {
		if call.Verb != "exec" {
			continue
		}
		args := execArgs{}
		if err := json.Unmarshal(call.Args, &args); err != nil {
			t.Fatalf("args: %v", err)
		}
		if slices.Contains(args.Argv, "restore") {
			restoreArgv = args.Argv
		}
	}
	for _, want := range []string{
		"--type=time",
		"--target=2026-07-30 14:32:00.000000+00", // RFC 3339 Z converted to pgbackrest's form
		"--target-action=promote",
	} {
		if !slices.Contains(restoreArgv, want) {
			t.Errorf("restore argv %v missing %q", restoreArgv, want)
		}
	}
}

// startFailsBeforeTarget simulates a restore whose WAL archive ends before
// the requested pitr target: the server refuses to start and the FATAL
// reaches stderr via the log tail.
func startFailsBeforeTarget(t *testing.T) func(verbCall) (any, *protoError) {
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			return putFileValue{}, nil
		}
		args := execArgs{}
		if err := json.Unmarshal(call.Args, &args); err != nil {
			t.Fatalf("args: %v", err)
		}
		switch {
		case isPGDataProbe(args):
			return outExec(sandboxPGData), nil
		case args.Argv[0] == "pg_isready":
			return okExec(2), nil // idle
		case args.Argv[0] == "sh" && strings.Contains(args.Argv[2], "pg_ctl"):
			return errExec(1, "pg_ctl: could not start server\nFATAL:  recovery ended before configured recovery target was reached"), nil
		default:
			return okExec(0), nil
		}
	}
}

// execScript returns the shell script an exec call carries, or "" for
// anything that is not one.
func execScript(t *testing.T, call verbCall) string {
	t.Helper()
	if call.Verb != "exec" {
		return ""
	}
	args := execArgs{}
	if err := json.Unmarshal(call.Args, &args); err != nil {
		t.Fatalf("args: %v", err)
	}
	if len(args.Argv) > 2 {
		return args.Argv[2]
	}
	return ""
}

// watchScripts wraps a handler, recording every script it is asked to run.
func watchScripts(t *testing.T, inner func(verbCall) (any, *protoError), scripts *[]string) func(verbCall) (any, *protoError) {
	return func(call verbCall) (any, *protoError) {
		if script := execScript(t, call); script != "" {
			*scripts = append(*scripts, script)
		}
		return inner(call)
	}
}

// answerPGData wraps a handler, making the sandbox report dir as its data
// directory.
func answerPGData(t *testing.T, inner func(verbCall) (any, *protoError), dir string) func(verbCall) (any, *protoError) {
	return func(call verbCall) (any, *protoError) {
		if script := execScript(t, call); strings.Contains(script, "PGDATA") {
			return outExec(dir), nil
		}
		return inner(call)
	}
}

// The three tests below pin the fix for the trap PostgreSQL 18 set: the
// official image moved PGDATA from /var/lib/postgresql/data to
// /var/lib/postgresql/18/docker, and the old path does not fail loudly on
// 18. pgbackrest restores into a directory the server will never read, the
// server starts on a cluster the backup never touched, and the drill
// reports a green that proves nothing — the one outcome this adapter must
// never produce.

func TestPhysicalRestoreUsesTheDirectoryTheSandboxNamed(t *testing.T) {
	var scripts []string
	line, _, _ := driveOp(t, "provision", pgbackrestPayload(writeRepoFixture(t), "demo"),
		watchScripts(t, physicalHandler(t, new([]string)), &scripts))
	if f := parseFinal(t, line); !f.OK {
		t.Fatalf("final = %+v", f)
	}
	if !strings.Contains(strings.Join(scripts, "\n"), sandboxPGData) {
		t.Errorf("no script mentions %s:\n%s", sandboxPGData, strings.Join(scripts, "\n"))
	}
	// The pre-18 path may survive only as the probe's own fallback.
	for _, script := range scripts {
		if strings.Contains(script, legacyPGDataDir) && !strings.Contains(script, "PGDATA") {
			t.Errorf("a script still hardcodes the pre-18 directory: %s", script)
		}
	}
}

func TestPGDataProbeFallsBackToThePre18Path(t *testing.T) {
	var scripts []string
	driveOp(t, "provision", pgbackrestPayload(writeRepoFixture(t), "demo"),
		watchScripts(t, physicalHandler(t, new([]string)), &scripts))
	var probe string
	for _, script := range scripts {
		if strings.Contains(script, "PGDATA") {
			probe = script
		}
	}
	if !strings.Contains(probe, legacyPGDataDir) {
		t.Errorf("probe = %q, want it to default to the pre-18 path for images that set no PGDATA", probe)
	}
}

func TestPhysicalRestoreRefusesADataDirectoryItWillNotWriteTo(t *testing.T) {
	repo := writeRepoFixture(t)
	// An empty answer, a relative path, and two that would carry shell
	// syntax into a script the adapter builds.
	for _, answer := range []string{"", "relative/path", "/tmp/$(id)", "/tmp/a b"} {
		t.Run(answer, func(t *testing.T) {
			line, _, _ := driveOp(t, "provision", pgbackrestPayload(repo, "demo"),
				answerPGData(t, physicalHandler(t, new([]string)), answer))
			f := parseFinal(t, line)
			if f.OK {
				t.Fatalf("accepted PGDATA %q", answer)
			}
			if !strings.Contains(f.Error.Message, "PGDATA") {
				t.Errorf("message = %q, want it to name the setting", f.Error.Message)
			}
		})
	}
}

func TestPITRRequestHandling(t *testing.T) {
	repo := writeRepoFixture(t)

	t.Run("malformed target_time refused", func(t *testing.T) {
		line, _, _ := driveOp(t, "provision", pitrPayload(repo, "yesterday 14:32"), mustNotBeCalled(t))
		f := parseFinal(t, line)
		if f.OK || f.Error.Code != "invalid_request" || !strings.Contains(f.Error.Message, "RFC 3339") {
			t.Errorf("final = %+v, want invalid_request about RFC 3339", f)
		}
	})

	t.Run("logical source kind refused", func(t *testing.T) {
		payload := `{"source":{"kind":"pgdump","path":"/nonexistent.dump"},"pitr":{"target_time":"2026-07-30T14:32:00Z"}}`
		line, _, _ := driveOp(t, "provision", payload, mustNotBeCalled(t))
		f := parseFinal(t, line)
		if f.OK || f.Error.Code != "invalid_request" || !strings.Contains(f.Error.Message, "pgbackrest") {
			t.Errorf("final = %+v, want invalid_request naming pgbackrest", f)
		}
	})

	t.Run("unreachable target is a restore verdict", func(t *testing.T) {
		line, _, _ := driveOp(t, "provision", pitrPayload(repo, "2026-07-30T14:32:00Z"), startFailsBeforeTarget(t))
		f := parseFinal(t, line)
		if f.OK || f.Error.Code != "restore_failed" || !strings.Contains(f.Error.Message, "not reachable") {
			t.Errorf("final = %+v, want restore_failed about an unreachable target", f)
		}
	})
}

// TestRepoWithAnUnreadableManifest pins the fail-closed half: this
// fixture's backup.info is a placeholder rather than a real manifest, so
// nothing dates the repository and nothing is invented from the newest
// file's mtime, which dates a copy. The readable case — epoch seconds
// straight out of a real manifest — lives in repotime_test.go.
func TestRepoWithAnUnreadableManifest(t *testing.T) {
	repo := writeRepoFixture(t)
	newest := time.Date(2026, 7, 30, 6, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(repo, "archive/demo/wal-001"), newest, newest); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	src, perr := resolveSource(context.Background(), "pgbackrest", repo, map[string]string{"stanza": "demo"})
	if perr != nil {
		t.Fatalf("resolveSource: %+v", perr)
	}
	if src.createdAt != nil {
		t.Errorf("createdAt = %s, want none — an mtime is not the backup's creation time", *src.createdAt)
	}
}

// writeVersionedRepoFixture writes a repository whose manifest carries a
// real [db] section, so the version pre-check has something to read.
func writeVersionedRepoFixture(t *testing.T, version string) string {
	t.Helper()
	repo := writeRepoFixture(t)
	manifest := "[backup:current]\n" +
		`20260810-003749F={"backup-timestamp-start":1786289869,"backup-timestamp-stop":1786289873}` + "\n\n" +
		"[db]\ndb-version=\"" + version + "\"\n"
	if err := os.WriteFile(filepath.Join(repo, "backup/demo/backup.info"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	return repo
}

// versionAnswering wraps physicalHandler with an answer for the engine
// version probe; an empty out simulates an image whose PATH has no
// postgres binary.
func versionAnswering(t *testing.T, sequence *[]string, out string) func(verbCall) (any, *protoError) {
	inner := physicalHandler(t, sequence)
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "exec" {
			args := execArgs{}
			if err := json.Unmarshal(call.Args, &args); err != nil {
				t.Fatalf("exec args: %v", err)
			}
			if args.Argv[0] == "postgres" {
				*sequence = append(*sequence, "postgres --version")
				if out == "" {
					return errExec(127, "postgres: not found"), nil
				}
				return outExec(out), nil
			}
		}
		return inner(call)
	}
}

// TestRepoIsDatedByItsManifest proves the wiring: a repository carrying a
// real manifest dates itself, and — unlike every other kind here — needs
// no declared zone, because pgBackRest records epoch seconds.
func TestRepoIsDatedByItsManifest(t *testing.T) {
	repo := writeRepoFixture(t)
	manifest := "[backup:current]\n" +
		`20260810-003749F={"backup-timestamp-start":1786289869,"backup-timestamp-stop":1786289873}` + "\n"
	if err := os.WriteFile(filepath.Join(repo, "backup/demo/backup.info"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	src, perr := resolveSource(context.Background(), "pgbackrest", repo, map[string]string{"stanza": "demo"})
	if perr != nil {
		t.Fatalf("resolveSource: %+v", perr)
	}
	if src.createdAt == nil || *src.createdAt != "2026-08-09T15:37:53.000Z" {
		t.Errorf("createdAt = %v, want the repository's own completion instant", src.createdAt)
	}
}

// driveVersionedProvision runs a pgbackrest provision against a repository
// whose manifest states version, with the engine answering out.
func driveVersionedProvision(t *testing.T, version, out string) (finalResponse, string) {
	t.Helper()
	repo := writeVersionedRepoFixture(t, version)
	var sequence []string
	line, _, _ := driveOp(t, "provision", pgbackrestPayload(repo, "demo"),
		versionAnswering(t, &sequence, out))
	return parseFinal(t, line), strings.Join(sequence, " | ")
}

func TestPhysicalVersionPrecheckRefusesAMismatch(t *testing.T) {
	f, sequence := driveVersionedProvision(t, "16", "postgres (PostgreSQL) 15.7 (Debian 15.7-1.pgdg120+1)")
	if f.OK || f.Error.Code != "invalid_request" {
		t.Fatalf("final = %+v, want invalid_request", f)
	}
	for _, want := range []string{"PostgreSQL 16 backup", "PostgreSQL 15"} {
		if !strings.Contains(f.Error.Message, want) {
			t.Errorf("message %q missing %q", f.Error.Message, want)
		}
	}
	if strings.Contains(sequence, "put_file") {
		t.Error("a refused pairing must not transfer the repository")
	}
}

func TestPhysicalVersionPrecheckPasses(t *testing.T) {
	t.Run("match proceeds and runs before the transfer", func(t *testing.T) {
		f, sequence := driveVersionedProvision(t, "16", "postgres (PostgreSQL) 16.9 (Debian 16.9-1.pgdg120+1)")
		if !f.OK {
			t.Fatalf("final = %+v", f)
		}
		if !strings.Contains(sequence, "postgres --version") {
			t.Fatalf("sequence %q missing the version probe", sequence)
		}
		if strings.Index(sequence, "postgres --version") > strings.Index(sequence, "put_file") {
			t.Error("the version pre-check must run before the repo transfer")
		}
	})
	t.Run("unanswerable engine version skips the check", func(t *testing.T) {
		f, _ := driveVersionedProvision(t, "16", "")
		if !f.OK {
			t.Fatalf("final = %+v — a refusal needs positive evidence, not a failed probe", f)
		}
	})
	t.Run("unreadable manifest skips the check", func(t *testing.T) {
		repo := writeRepoFixture(t) // placeholder backup.info: no version to read
		var sequence []string
		line, _, _ := driveOp(t, "provision", pgbackrestPayload(repo, "demo"), physicalHandler(t, &sequence))
		if f := parseFinal(t, line); !f.OK {
			t.Fatalf("final = %+v", f)
		}
		if strings.Contains(strings.Join(sequence, " | "), "postgres --version") {
			t.Error("no version in the manifest — the engine must not be queried")
		}
	})
}
