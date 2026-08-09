package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// restorePath is where the drill's restore reads from; candidates are
// probed through the scratch path beside it.
const restorePath = "/scratch/probavi-restore.bak"

// scanHandler drives a directory scan: every candidate is probed through
// one sandbox path, so the fake core remembers which file was transferred
// last and answers the following HEADERONLY probe for that file. What
// lands on the restore path is the artifact the drill chose.
type scanHandler struct {
	t *testing.T
	// headers maps a candidate's base name to the rows its probe returns.
	headers map[string]string
	// probeFails maps a candidate's base name to a HEADERONLY stderr; the
	// probe then exits non-zero.
	probeFails map[string]string
	// probed is every candidate the scan examined, in order; transferred is
	// what was moved into place for the restore.
	probed      []string
	transferred []string
	lastPut     string
	restoreArgv []string
}

func (h *scanHandler) handle(call verbCall) (any, *protoError) {
	if call.Verb == "put_file" {
		args := putFileArgs{}
		if err := json.Unmarshal(call.Args, &args); err != nil {
			h.t.Fatalf("put_file args: %v", err)
		}
		switch args.DestPath {
		case restorePath + probeSuffix:
			h.lastPut = filepath.Base(args.SourcePath)
			h.probed = append(h.probed, h.lastPut)
		case restorePath:
			h.lastPut = filepath.Base(args.SourcePath)
			h.transferred = append(h.transferred, h.lastPut)
		}
		return putFileValue{BytesCopied: 10, DurationSeconds: 0.5}, nil
	}
	args, kind := classify(h.t, call)
	switch kind {
	case "initfile", "probe":
		return servingExec(), nil
	case "headeronly":
		if stderr, ok := h.probeFails[h.lastPut]; ok {
			return errExec(1, stderr), nil
		}
		rows, ok := h.headers[h.lastPut]
		if !ok {
			h.t.Fatalf("no scripted header for %s", h.lastPut)
		}
		return execValue{ExitCode: 0, DurationSeconds: 0.05,
			StdoutB64: base64.StdEncoding.EncodeToString([]byte(rows))}, nil
	case "restore":
		h.restoreArgv = args.Argv
		return execValue{ExitCode: 0, DurationSeconds: 1.5}, nil
	}
	h.t.Fatalf("unexpected exec: %v", args.Argv)
	return nil, nil
}

func dirPayload(dir string) string {
	return fmt.Sprintf(`{"source":{"kind":"bak_dir","path":%q,"params":{},"credential_env":[]},"sandbox":{"scratch_dir":"/scratch"},"options":{}}`, dir)
}

// TestScanSkipsNonFullCandidates is the heart of #88: the newest file is a
// transaction log backup, and the drill must walk past it to the full
// backup rather than failing on a healthy backup set.
func TestScanSkipsNonFullCandidates(t *testing.T) {
	dir := t.TempDir()
	full := writeMedia(t, dir, "a-full.bak", "FULL-BYTES")
	diff := writeMedia(t, dir, "m-diff.bak", "DIFF")
	logNewest := writeMedia(t, dir, "z-log.trn", "LOG")
	touch(t, full, 3*time.Hour)
	touch(t, diff, 2*time.Hour)
	touch(t, logNewest, time.Hour)

	h := &scanHandler{t: t, headers: map[string]string{
		"z-log.trn":  headerRow(backupTypeLog, 1) + "\n",
		"m-diff.bak": headerRow(backupTypeDifferential, 1) + "\n",
		"a-full.bak": headerRow(backupTypeFull, 1) + "\n",
	}}
	line, _, exit := driveOp(t, "provision", dirPayload(dir), h.handle)
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v — a directory with a full backup must restore", exit, f)
	}
	want := []string{"z-log.trn", "m-diff.bak", "a-full.bak"}
	if strings.Join(h.probed, ",") != strings.Join(want, ",") {
		t.Errorf("probed = %v, want every candidate examined, newest file first %v", h.probed, want)
	}
	if strings.Join(h.transferred, ",") != "a-full.bak" {
		t.Errorf("transferred = %v, want only the chosen full backup moved into place", h.transferred)
	}

	// The identity must describe the artifact the engine chose, not the
	// newest file: that is what an auditor reads.
	res := provisionWire{}
	if err := json.Unmarshal(f.Payload, &res); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if res.SourceIdentity.SizeBytes != int64(len("TAPEFULL-BYTES")) {
		t.Errorf("size_bytes = %d, want the chosen full backup's size", res.SourceIdentity.SizeBytes)
	}
	if res.State["backup_set"] != "1" {
		t.Errorf("state = %+v, want the chosen set recorded", res.State)
	}
}

// TestScanRefusesCorruptCandidate pins the rule that keeps a drill honest:
// backup media the engine cannot read fails the drill instead of quietly
// falling back to an older backup, which would hide exactly what the drill
// exists to surface.
func TestScanRefusesCorruptCandidate(t *testing.T) {
	dir := t.TempDir()
	older := writeMedia(t, dir, "a-full.bak", "FULL-BYTES")
	newest := writeMedia(t, dir, "z-newest.bak", "CORRUPT")
	touch(t, older, time.Hour)
	touch(t, newest, time.Minute)

	h := &scanHandler{t: t,
		headers: map[string]string{"a-full.bak": headerRow(backupTypeFull, 1) + "\n"},
		probeFails: map[string]string{
			"z-newest.bak": "Msg 3241, Level 16, State 1, Server x, Line 1\n" +
				"The media family on device '/scratch/probavi-restore.bak' is incorrectly formed. " +
				"SQL Server cannot process this media family.",
		}}
	line, _, exit := driveOp(t, "provision", dirPayload(dir), h.handle)
	f := parseFinal(t, line)
	if exit != 0 || f.OK {
		t.Fatalf("exit=%d final=%+v, want a failure", exit, f)
	}
	if f.Error.Code != "source_corrupt" {
		t.Errorf("code = %s (%s), want source_corrupt", f.Error.Code, f.Error.Message)
	}
	if len(h.probed) != 1 {
		t.Errorf("probed = %v, want the scan to stop at the unreadable newest backup", h.probed)
	}
	if len(h.transferred) != 0 {
		t.Errorf("transferred = %v, want nothing restored after a refusal", h.transferred)
	}
}

// TestScanExhaustedSaysWhy proves the verdict is actionable: a directory of
// log backups is not restorable, and the drill names what it examined
// instead of quoting the engine's Msg 3118 at the operator.
func TestScanExhaustedSaysWhy(t *testing.T) {
	dir := t.TempDir()
	writeMedia(t, dir, "a-log.trn", "LOG1")
	writeMedia(t, dir, "b-log.trn", "LOG2")
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte("abc  a-log.trn\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	h := &scanHandler{t: t, headers: map[string]string{
		"a-log.trn": headerRow(backupTypeLog, 1) + "\n",
		"b-log.trn": headerRow(backupTypeLog, 1) + "\n",
	}}
	line, _, _ := driveOp(t, "provision", dirPayload(dir), h.handle)
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "source_not_found" {
		t.Fatalf("final = %+v, want source_not_found", f)
	}
	for _, want := range []string{"no full backup", "transaction log backup", "SHA256SUMS"} {
		if !strings.Contains(f.Error.Message, want) {
			t.Errorf("message = %q, want it to carry %q", f.Error.Message, want)
		}
	}
}

// TestNamedFileWithoutFullBackup covers the bak kind: the operator named
// this file, so the answer is about this file — and it explains what the
// file actually is rather than repeating the engine's "database does not
// exist" complaint.
func TestNamedFileWithoutFullBackup(t *testing.T) {
	fixture := writeFixture(t, "TAPElog-bytes")
	h := &scanHandler{t: t, headers: map[string]string{
		filepath.Base(fixture): headerRow(backupTypeLog, 1) + "\n",
	}}
	line, _, exit := driveOp(t, "provision", provisionPayload(fixture, "{}"), h.handle)
	f := parseFinal(t, line)
	if exit != 0 || f.OK {
		t.Fatalf("exit=%d final=%+v, want a refusal", exit, f)
	}
	if f.Error.Code != "invalid_request" {
		t.Errorf("code = %s (%s), want invalid_request", f.Error.Code, f.Error.Message)
	}
	for _, want := range []string{"transaction log backup", "cannot create a database"} {
		if !strings.Contains(f.Error.Message, want) {
			t.Errorf("message = %q, want it to carry %q", f.Error.Message, want)
		}
	}
}

// TestMultiSetRestoresNewestFull covers appended media: restoring without
// naming a set takes the first one on the file, which is the oldest backup
// on it, so the newest full set is named explicitly.
func TestMultiSetRestoresNewestFull(t *testing.T) {
	fixture := writeFixture(t, "TAPEmulti")
	h := &scanHandler{t: t, headers: map[string]string{
		filepath.Base(fixture): headerRow(backupTypeFull, 1) + "\n" +
			headerRow(backupTypeLog, 2) + "\n" +
			headerRow(backupTypeFull, 3) + "\n",
	}}
	line, _, exit := driveOp(t, "provision", provisionPayload(fixture, "{}"), h.handle)
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v", exit, f)
	}
	if len(h.restoreArgv) == 0 || h.restoreArgv[len(h.restoreArgv)-1] != "3" {
		t.Errorf("restore argv = %v, want the newest full set named last", h.restoreArgv)
	}
	res := provisionWire{}
	if err := json.Unmarshal(f.Payload, &res); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if res.State["backup_set"] != "3" {
		t.Errorf("state = %+v, want the chosen set recorded", res.State)
	}
}

// TestRestoreScriptNamesTheBackupSet pins the T-SQL itself. Asserting the
// argv alone would compare the script against itself; both statements must
// carry the set number, or the engine silently restores the first set on
// the media.
func TestRestoreScriptNamesTheBackupSet(t *testing.T) {
	for _, want := range []string{
		"RESTORE FILELISTONLY FROM DISK = N'$1' WITH FILE = $3",
		"RESTORE DATABASE [$2] FROM DISK = N'$1' WITH FILE = $3, RECOVERY",
	} {
		if !strings.Contains(restoreScript, want) {
			t.Errorf("restoreScript does not carry %q — the chosen backup set must be named", want)
		}
	}
}

// TestUnclassifiableHeaderFallsBackToTheEngineDefault pins the protocol's
// simulated-sandbox contract (§10): when every verb succeeds, provision
// must succeed. An engine that answers without classifying the media says
// nothing about it, so the adapter restores the set the engine itself
// would have — it does not invent a verdict either way.
func TestUnclassifiableHeaderFallsBackToTheEngineDefault(t *testing.T) {
	fixture := writeFixture(t, "TAPEbytes")
	h := &scanHandler{t: t, headers: map[string]string{
		filepath.Base(fixture): "1\n", // what the simulated sandbox returns
	}}
	line, _, exit := driveOp(t, "provision", provisionPayload(fixture, "{}"), h.handle)
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v — a sandbox whose verbs all succeed must provision", exit, f)
	}
	if len(h.restoreArgv) == 0 || h.restoreArgv[len(h.restoreArgv)-1] != "1" {
		t.Errorf("restore argv = %v, want the engine's own default set", h.restoreArgv)
	}
}

// TestSelectionTimeIsNotRecoveryTime pins decision (b): probing rejected
// candidates is how the drill finds the backup, not part of the recovery
// it measures — but the chosen artifact's transfer is.
func TestSelectionTimeIsNotRecoveryTime(t *testing.T) {
	dir := t.TempDir()
	full := writeMedia(t, dir, "a-full.bak", "FULL")
	logNewest := writeMedia(t, dir, "z-log.trn", "LOG")
	touch(t, full, time.Hour)
	touch(t, logNewest, time.Minute)

	h := &scanHandler{t: t, headers: map[string]string{
		"z-log.trn":  headerRow(backupTypeLog, 1) + "\n",
		"a-full.bak": headerRow(backupTypeFull, 1) + "\n",
	}}
	line, _, _ := driveOp(t, "provision", dirPayload(dir), h.handle)
	f := parseFinal(t, line)
	if !f.OK {
		t.Fatalf("final = %+v", f)
	}
	res := provisionWire{}
	if err := json.Unmarshal(f.Payload, &res); err != nil {
		t.Fatalf("payload: %v", err)
	}
	// Three transfers happened at 0.5 s each — two probes and the chosen
	// artifact's — and only the last one is recovery.
	if res.Timings.Transfer != 0.5 {
		t.Errorf("transfer_seconds = %v, want the chosen artifact's transfer alone", res.Timings.Transfer)
	}
}

// TestScanRanksByTheHeaderNotTheFileTime is issue #100 for this adapter:
// the newest *file* is an older backup that was copied into the directory
// afterwards, and the drill must restore the backup the server finished
// last. Both candidates are full backups, so nothing but the recorded
// completion time can tell them apart.
func TestScanRanksByTheHeaderNotTheFileTime(t *testing.T) {
	dir := t.TempDir()
	fresh := writeMedia(t, dir, "a-fresh.bak", "FRESH-BYTES")
	stale := writeMedia(t, dir, "z-stale-copy.bak", "STALE")
	touch(t, fresh, 2*time.Hour) // taken last, but copied in first
	touch(t, stale, time.Minute) // copied in last, so it looks newest

	h := &scanHandler{t: t, headers: map[string]string{
		"z-stale-copy.bak": headerRowWithDate("2026-08-01 03:00:00.000") + "\n",
		"a-fresh.bak":      headerRowWithDate("2026-08-09 03:00:00.000") + "\n",
	}}
	line, _, exit := driveOp(t, "provision", dirPayload(dir), h.handle)
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v", exit, f)
	}
	if strings.Join(h.transferred, ",") != "a-fresh.bak" {
		t.Errorf("transferred = %v, want the backup the server finished last — a copy's file time must not decide",
			h.transferred)
	}
	res := provisionWire{}
	if err := json.Unmarshal(f.Payload, &res); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if res.SourceIdentity.SizeBytes != int64(len("TAPEFRESH-BYTES")) {
		t.Errorf("size_bytes = %d, want the chosen backup's size", res.SourceIdentity.SizeBytes)
	}
}

// TestScanRanksUndatedMediaByFileTime keeps the old rule where the new one
// has nothing to work with: headers that carry no completion date leave the
// scan order — newest file first — deciding, and a backup that can be dated
// outranks one that cannot.
func TestScanRanksUndatedMediaByFileTime(t *testing.T) {
	t.Run("no header carries a date", func(t *testing.T) {
		dir := t.TempDir()
		older := writeMedia(t, dir, "a-older.bak", "OLD")
		newest := writeMedia(t, dir, "z-newest.bak", "NEW")
		touch(t, older, 2*time.Hour)
		touch(t, newest, time.Minute)

		h := &scanHandler{t: t, headers: map[string]string{
			"a-older.bak":  headerRow(backupTypeFull, 1) + "\n",
			"z-newest.bak": headerRow(backupTypeFull, 1) + "\n",
		}}
		line, _, _ := driveOp(t, "provision", dirPayload(dir), h.handle)
		if f := parseFinal(t, line); !f.OK {
			t.Fatalf("final = %+v", f)
		}
		if strings.Join(h.transferred, ",") != "z-newest.bak" {
			t.Errorf("transferred = %v, want the newest file when no header can rank them", h.transferred)
		}
	})

	t.Run("a dated backup outranks an undated one", func(t *testing.T) {
		dir := t.TempDir()
		dated := writeMedia(t, dir, "a-dated.bak", "DATED")
		undated := writeMedia(t, dir, "z-undated.bak", "PLAIN")
		touch(t, dated, 2*time.Hour)
		touch(t, undated, time.Minute)

		h := &scanHandler{t: t, headers: map[string]string{
			"a-dated.bak":   headerRowWithDate("2026-08-01 03:00:00.000") + "\n",
			"z-undated.bak": headerRow(backupTypeFull, 1) + "\n",
		}}
		line, _, _ := driveOp(t, "provision", dirPayload(dir), h.handle)
		if f := parseFinal(t, line); !f.OK {
			t.Fatalf("final = %+v", f)
		}
		if strings.Join(h.transferred, ",") != "a-dated.bak" {
			t.Errorf("transferred = %v, want the backup the drill can also date", h.transferred)
		}
	})
}

// TestMediaRankingIsATotalOrder pins the tie-breaking that keeps the choice
// from depending on directory iteration order.
func TestMediaRankingIsATotalOrder(t *testing.T) {
	early := time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)
	late := early.Add(time.Hour)
	tests := []struct {
		name string
		a, b mediaRank
	}{
		{"later completion wins",
			mediaRank{clock: late, dated: true, index: 3},
			mediaRank{clock: early, dated: true, index: 0}},
		{"dated beats undated even when older",
			mediaRank{clock: early, dated: true, index: 3},
			mediaRank{index: 0}},
		{"the same completion falls through to the scan order",
			mediaRank{clock: late, dated: true, index: 0},
			mediaRank{clock: late, dated: true, index: 1}},
		{"undated media falls through to the scan order",
			mediaRank{index: 0}, mediaRank{index: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.a.beats(tt.b) {
				t.Error("a.beats(b) = false, want a to win")
			}
			if tt.b.beats(tt.a) {
				t.Error("b.beats(a) too — the ranking must be a strict order")
			}
		})
	}
	same := mediaRank{clock: late, dated: true, index: 2}
	if same.beats(same) {
		t.Error("a rank beats itself — the ranking is not strict")
	}
}
