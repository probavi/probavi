package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// chainHeaderRow is a HEADERONLY row carrying the columns a chain is built
// from, at the positions a real 59-column row puts them.
func chainHeaderRow(kind int, database, first, last, checkpoint, dbLSN, finished string) string {
	fields := make([]string, 20)
	for i := range fields {
		fields[i] = "x"
	}
	fields[0], fields[1] = "NULL", "NULL"
	fields[headerTypeIndex] = fmt.Sprintf("%d", kind)
	fields[headerPositionIdx] = "1"
	fields[headerDatabaseIdx] = database
	fields[headerFirstLSNIdx] = first
	fields[headerLastLSNIdx] = last
	fields[headerCheckpointIdx] = checkpoint
	fields[headerDatabaseLSNIdx] = dbLSN
	fields[headerFinishIdx] = finished
	return strings.Join(fields, "|")
}

// chainDir writes the measured directory as backup media on the host and
// returns the header each file answers with.
func chainDir(t *testing.T) (string, map[string]string) {
	t.Helper()
	dir := t.TempDir()
	headers := map[string]string{
		"01-full.bak": chainHeaderRow(backupTypeFull, "shop", lsnFullFirst, lsnFullLast, lsnAnchor, "0",
			"2026-08-09 21:00:01.000"),
		"05-diff.bak": chainHeaderRow(backupTypeDifferential, "shop", "42000000065600001", lsnDiff5Last,
			"42000000065600001", lsnAnchor, "2026-08-09 21:00:05.000"),
		"06-log.trn": chainHeaderRow(backupTypeLog, "shop", lsnLog6First, lsnLog6Last,
			"42000000065600001", lsnAnchor, "2026-08-09 21:00:06.000"),
		"07-log.trn": chainHeaderRow(backupTypeLog, "shop", lsnLog7First, lsnLog7Last,
			"42000000069600002", lsnAnchor, "2026-08-09 21:00:07.000"),
	}
	for name := range headers {
		writeMedia(t, dir, name, "payload-"+name)
	}
	return dir, headers
}

// chainRun records what the adapter did, keyed the way the fake core sees
// it: which file went to which sandbox path, and how each was restored.
type chainRun struct {
	t          *testing.T
	headers    map[string]string
	probed     []string
	transfers  map[string]string // sandbox path -> source base name
	restores   []string          // "<verb> <path> <recovery> moves=<0|1>"
	lastProbed string
	// failAt names the member whose restore should fail, with failWith on
	// stderr — the shape a real broken chain produces.
	failAt   string
	failWith string
}

func (r *chainRun) handle(call verbCall) (any, *protoError) {
	if call.Verb == "put_file" {
		args := putFileArgs{}
		if err := json.Unmarshal(call.Args, &args); err != nil {
			r.t.Fatalf("put_file args: %v", err)
		}
		base := filepath.Base(args.SourcePath)
		if strings.Contains(args.DestPath, "probe") {
			r.probed = append(r.probed, base)
			r.lastProbed = base
		} else {
			r.transfers[args.DestPath] = base
		}
		return putFileValue{BytesCopied: 10, DurationSeconds: 0.25}, nil
	}
	args, kind := classify(r.t, call)
	switch kind {
	case "initfile", "probe":
		return servingExec(), nil
	case "headeronly":
		row, ok := r.headers[r.lastProbed]
		if !ok {
			r.t.Fatalf("no scripted header for %s", r.lastProbed)
		}
		return execValue{ExitCode: 0, StdoutB64: base64.StdEncoding.EncodeToString([]byte(row + "\n"))}, nil
	case "chain":
		// argv: sh -c script sh <path> <db> <file> <verb> <recovery> <moves>
		member := r.transfers[args.Argv[4]]
		r.restores = append(r.restores,
			fmt.Sprintf("%s %s %s moves=%s", args.Argv[7], member, args.Argv[8], args.Argv[9]))
		if member == r.failAt {
			return errExec(1, r.failWith), nil
		}
		return execValue{ExitCode: 0, DurationSeconds: 0.5}, nil
	}
	r.t.Fatalf("unexpected exec: %v", args.Argv)
	return nil, nil
}

func chainPayload(dir string) string {
	return chainPayloadWithParams(dir, "{}")
}

func chainPayloadWithParams(dir, params string) string {
	return fmt.Sprintf(`{"source":{"kind":"bak_chain","path":%q,"params":%s,"credential_env":[]},"sandbox":{"scratch_dir":"/scratch"},"options":{"database":"shop"}}`,
		dir, params)
}

// TestProvisionChain is the shape of the whole kind: probe everything,
// restore the chain in order, recover exactly once at the end.
func TestProvisionChain(t *testing.T) {
	dir, headers := chainDir(t)
	run := &chainRun{t: t, headers: headers, transfers: map[string]string{}}
	line, _, exit := driveOp(t, "provision", chainPayload(dir), run.handle)
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	f := parseFinal(t, line)
	if !f.OK {
		t.Fatalf("final = %+v", f)
	}

	// Every candidate is probed once, through the shared probe path, so
	// the sandbox never holds the whole directory at once.
	if len(run.probed) != 4 {
		t.Errorf("probed = %v, want every candidate once", run.probed)
	}
	want := []string{
		"DATABASE 01-full.bak NORECOVERY moves=1",
		"DATABASE 05-diff.bak NORECOVERY moves=0",
		"LOG 06-log.trn NORECOVERY moves=0",
		"LOG 07-log.trn RECOVERY moves=0",
	}
	if strings.Join(run.restores, " | ") != strings.Join(want, " | ") {
		t.Errorf("restores =\n  %s\nwant\n  %s",
			strings.Join(run.restores, "\n  "), strings.Join(want, "\n  "))
	}
}

func TestProvisionChainResult(t *testing.T) {
	dir, headers := chainDir(t)
	run := &chainRun{t: t, headers: headers, transfers: map[string]string{}}
	line, _, _ := driveOp(t, "provision",
		chainPayload(dir), run.handle)
	f := parseFinal(t, line)
	if !f.OK {
		t.Fatalf("final = %+v", f)
	}
	res := provisionWire{}
	if err := json.Unmarshal(f.Payload, &res); err != nil {
		t.Fatalf("payload: %v", err)
	}

	// The identity covers every member in restore order, framed by role
	// and size — a checksum blind to the logs would let the data the
	// drill actually recovered change without the record noticing.
	h := sha256.New()
	for i, member := range []struct{ name, role string }{
		{"01-full.bak", "full backup"},
		{"05-diff.bak", "differential backup"},
		{"06-log.trn", "transaction log backup"},
		{"07-log.trn", "transaction log backup"},
	} {
		body := "TAPEpayload-" + member.name
		fmt.Fprintf(h, "%d:%s\x00%d\x00%s", i, member.role, len(body), body)
	}
	if want := "sha256:" + hex.EncodeToString(h.Sum(nil)); res.SourceIdentity.Checksum != want {
		t.Errorf("checksum = %s, want the whole chain hashed in restore order", res.SourceIdentity.Checksum)
	}

	// Unlike the probing that finds them, every member's transfer and
	// restore is the recovery this drill measures.
	if res.Timings.Transfer != 4*0.25 || res.Timings.Restore != 4*0.5 {
		t.Errorf("timings = %+v, want every member counted", res.Timings)
	}
	if res.State["chain_length"] != "4" || !strings.Contains(res.State["chain"], "07-log.trn") {
		t.Errorf("state = %+v, want the chain recorded in order", res.State)
	}
}

// TestProvisionChainIsDatedByItsLastMember pins what a chain's freshness
// means: the log backup the recovery ends on.
func TestProvisionChainIsDatedByItsLastMember(t *testing.T) {
	dir, headers := chainDir(t)
	run := &chainRun{t: t, headers: headers, transfers: map[string]string{}}
	line, _, _ := driveOp(t, "provision",
		chainPayloadWithParams(dir, `{"backup_timezone":"Asia/Tokyo"}`), run.handle)
	f := parseFinal(t, line)
	if !f.OK {
		t.Fatalf("final = %+v", f)
	}
	res := provisionWire{}
	if err := json.Unmarshal(f.Payload, &res); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if res.SourceIdentity.CreatedAt == nil || *res.SourceIdentity.CreatedAt != "2026-08-09T21:00:07.000+09:00" {
		t.Errorf("created_at = %v, want the last log's completion time", res.SourceIdentity.CreatedAt)
	}
}

func TestProvisionChainFailures(t *testing.T) {
	t.Run("a gap in the log sequence fails the drill", func(t *testing.T) {
		dir, headers := chainDir(t)
		// Remove the log that bridges the differential to the last one.
		if err := os.Remove(filepath.Join(dir, "06-log.trn")); err != nil {
			t.Fatal(err)
		}
		delete(headers, "06-log.trn")
		run := &chainRun{t: t, headers: headers, transfers: map[string]string{}}
		line, _, _ := driveOp(t, "provision", chainPayload(dir), run.handle)
		f := parseFinal(t, line)
		if f.OK || f.Error.Code != "source_not_found" {
			t.Fatalf("final = %+v, want source_not_found", f)
		}
		if !strings.Contains(f.Error.Message, "gap") {
			t.Errorf("message = %q, want the gap named", f.Error.Message)
		}
		if len(run.restores) != 0 {
			t.Errorf("restores = %v, want none — a chain is checked before anything is replayed", run.restores)
		}
	})

}

func TestProvisionChainDatabaseSelection(t *testing.T) {
	t.Run("several databases need the config to say which", func(t *testing.T) {
		dir, headers := chainDir(t)
		writeMedia(t, dir, "99-other.bak", "payload-99-other.bak")
		headers["99-other.bak"] = chainHeaderRow(backupTypeFull, "other", lsnFullFirst, lsnFullLast,
			lsnAnchor, "0", "2026-08-09 21:00:09.000")
		run := &chainRun{t: t, headers: headers, transfers: map[string]string{}}
		line, _, _ := driveOp(t, "provision", chainPayload(dir), run.handle)
		f := parseFinal(t, line)
		if f.OK || f.Error.Code != "invalid_request" {
			t.Fatalf("final = %+v, want invalid_request", f)
		}
		if !strings.Contains(f.Error.Message, "database_name") {
			t.Errorf("message = %q, want the option named", f.Error.Message)
		}
	})

	t.Run("the named database is restored from a mixed directory", func(t *testing.T) {
		dir, headers := chainDir(t)
		writeMedia(t, dir, "99-other.bak", "payload-99-other.bak")
		headers["99-other.bak"] = chainHeaderRow(backupTypeFull, "other", lsnFullFirst, lsnFullLast,
			lsnAnchor, "0", "2026-08-09 21:00:09.000")
		run := &chainRun{t: t, headers: headers, transfers: map[string]string{}}
		line, _, _ := driveOp(t, "provision",
			chainPayloadWithParams(dir, `{"database_name":"other"}`), run.handle)
		f := parseFinal(t, line)
		if !f.OK {
			t.Fatalf("final = %+v", f)
		}
		if len(run.restores) != 1 || !strings.Contains(run.restores[0], "99-other.bak") {
			t.Errorf("restores = %v, want only the named database's chain", run.restores)
		}
	})

	t.Run("a database_name that is not a name", func(t *testing.T) {
		dir, _ := chainDir(t)
		line, calls, _ := driveOp(t, "provision",
			chainPayloadWithParams(dir, `{"database_name":"x]; DROP DATABASE [master"}`),
			func(verbCall) (any, *protoError) { return nil, protoErr("internal", false, "must not be called") })
		f := parseFinal(t, line)
		if f.OK || f.Error.Code != "invalid_request" || len(calls) != 0 {
			t.Fatalf("final = %+v calls = %d, want invalid_request before any verb", f, len(calls))
		}
	})

}

func TestProvisionChainMemberFailure(t *testing.T) {
	t.Run("a member that fails to restore names itself", func(t *testing.T) {
		dir, headers := chainDir(t)
		run := &chainRun{t: t, headers: headers, transfers: map[string]string{},
			failAt: "06-log.trn",
			failWith: "Msg 4305, Level 16, State 1, Server x, Line 1\n" +
				"The log in this backup set begins at LSN 42000000057600001, which is too recent to apply to the database."}
		line, _, _ := driveOp(t, "provision", chainPayload(dir), run.handle)
		f := parseFinal(t, line)
		if f.OK || f.Error.Code != "restore_failed" {
			t.Fatalf("final = %+v, want restore_failed", f)
		}
		for _, want := range []string{"06-log.trn", "transaction log backup", "too recent"} {
			if !strings.Contains(f.Error.Message, want) {
				t.Errorf("message = %q, want it to carry %q", f.Error.Message, want)
			}
		}
	})
}

func TestChainNamesReadsInRestoreOrder(t *testing.T) {
	chain, perr := buildChain(realDirectory())
	if perr != nil {
		t.Fatalf("buildChain: %+v", perr)
	}
	want := "01-full.bak -> 05-diff.bak -> 06-log.trn -> 07-log.trn"
	if got := chainNames(chain); got != want {
		t.Errorf("chainNames = %q, want %q", got, want)
	}
}
