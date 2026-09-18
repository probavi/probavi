package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// answeredCore is a core whose next sandbox calls are answered as given,
// so a step can be driven straight to the branch under test. A core built
// with no answers is one whose stream is gone: the step then meets the
// harness's own failure, which is what it meets when the core goes away
// mid-operation.
func answeredCore(t *testing.T, answers ...any) *core {
	t.Helper()
	var lines strings.Builder
	for i, val := range answers {
		raw, err := json.Marshal(val)
		if err != nil {
			t.Fatal(err)
		}
		lines.WriteString(sandboxResultLine(
			`{"call_id":"c` + strconv.Itoa(i+1) + `","ok":true,"value":` + string(raw) + `}`))
	}
	return harnessCore(lines.String(), io.Discard)
}

// said is an exec answer carrying what the sandbox printed.
func said(exit int, stdout, stderr string) execValue {
	return execValue{
		ExitCode:        exit,
		StdoutB64:       base64.StdEncoding.EncodeToString([]byte(stdout)),
		StderrB64:       base64.StdEncoding.EncodeToString([]byte(stderr)),
		DurationSeconds: 0.25,
	}
}

// wantRefusal fails the test unless the refusal carries the code and the
// words that name what went wrong.
func wantRefusal(t *testing.T, perr *protoError, code, message string) {
	t.Helper()
	if perr == nil || perr.Code != code || !strings.Contains(perr.Message, message) {
		t.Errorf("got %+v, want %s mentioning %q", perr, code, message)
	}
}

func TestPreflightAndWorkDirsReportWhatTheSandboxSaid(t *testing.T) {
	t.Run("an idle wrapper image", func(t *testing.T) {
		if perr := checkSandbox(context.Background(), answeredCore(t, said(0, "", ""))); perr != nil {
			t.Errorf("perr = %+v, want the sandbox this adapter needs to pass", perr)
		}
	})
	t.Run("a sandbox already serving", func(t *testing.T) {
		perr := checkSandbox(context.Background(),
			answeredCore(t, said(1, "", "something is already serving on 8080 — the sandbox must start idle\n")))
		wantRefusal(t, perr, "invalid_request", "already serving")
	})
	t.Run("work directories that could not be made", func(t *testing.T) {
		perr := mkdirAll(context.Background(),
			answeredCore(t, said(1, "", "mkdir: can't create directory '/backups': Read-only file system\n")), "/backups")
		wantRefusal(t, perr, "internal", "Read-only file system")
	})
	t.Run("a core that went away mid-call", func(t *testing.T) {
		if perr := mkdirAll(context.Background(), answeredCore(t), "/backups"); perr == nil ||
			perr.Code != "internal" {
			t.Errorf("perr = %+v, want the harness's own failure", perr)
		}
	})
}

// TestTransferArchiveReadsTheBackupWhereItLanded: an archive holds the
// backup either at its root or one directory below, so the sandbox is
// asked where it landed, the backup's own metadata is read there, and the
// files are moved to the name that metadata states.
func TestTransferArchiveReadsTheBackupWhereItLanded(t *testing.T) {
	const workDir, backupsRoot = "/work", "/backups"
	meta := `{"id":"nightly","status":"SUCCESS","nodes":{"node1":{"classes":["Orders"],"status":"SUCCESS"}}}`
	put := putFileValue{BytesCopied: 4096, DurationSeconds: 0.5}
	t.Run("a backup one directory down", func(t *testing.T) {
		src := &resolvedSource{path: "/backups/nightly.tar", tarball: true}
		core := answeredCore(t,
			said(0, "", ""), put, said(0, "", ""), // mkdir, transfer, unpack
			said(0, workDir+"/extract/nightly\n", ""), // locate
			said(0, meta, ""),                         // read the metadata
			said(0, "", ""),                           // move it into place
		)
		seconds, perr := transferArchive(context.Background(), core, src, workDir, backupsRoot)
		if perr != nil {
			t.Fatalf("perr = %+v", perr)
		}
		if seconds <= 0 || src.meta == nil || src.meta.ID != "nightly" || src.node != "node1" {
			t.Errorf("transferArchive = %v, src = %+v, want the backup's own id and node", seconds, src)
		}
	})
	for name, tc := range map[string]struct {
		answers []any
		code    string
		message string
	}{
		"an archive tar refused": {
			[]any{said(0, "", ""), put, said(2, "", "tar: invalid tar magic\n")},
			"source_corrupt", "invalid tar magic",
		},
		"an archive holding no backup metadata": {
			[]any{said(0, "", ""), put, said(0, "", ""), said(0, "/work/extract\n", ""), said(1, "", "")},
			"source_corrupt", "POST /v1/backups/filesystem",
		},
		"a backup the sandbox could not place": {
			[]any{said(0, "", ""), put, said(0, "", ""), said(0, "/work/extract\n", ""),
				said(0, meta, ""), said(1, "", "mv: cannot move: Permission denied\n")},
			"internal", "place the backup",
		},
		"a core that went away mid-call": {nil, "internal", "closed the stream"},
	} {
		t.Run(name, func(t *testing.T) {
			_, perr := transferArchive(context.Background(), answeredCore(t, tc.answers...),
				&resolvedSource{path: "/backups/nightly.tar", tarball: true}, workDir, backupsRoot)
			wantRefusal(t, perr, tc.code, tc.message)
		})
	}
}

// TestAnArchiveWhoseMetadataDoesNotParseIsLeftToTheEngine: a real
// backup's metadata always parses, because the engine wrote it — and on a
// damaged one the engine refuses the restore in its own words, which is a
// better verdict than a guess made here.
func TestAnArchiveWhoseMetadataDoesNotParseIsLeftToTheEngine(t *testing.T) {
	src := &resolvedSource{path: "/backups/nightly.tar", tarball: true}
	seconds, perr := readArchiveMeta(context.Background(),
		answeredCore(t, said(0, "{not json", "")), src, "/work/extract")
	if perr != nil || seconds != 0.25 {
		t.Errorf("readArchiveMeta = %v, %+v, want the read to stand", seconds, perr)
	}
	if src.meta != nil {
		t.Errorf("src.meta = %+v, want nothing taken from metadata that did not parse", src.meta)
	}
}

// TestAnArchiveStatingAnIncompleteBackupIsRefused: the metadata inside
// the archive carries the same claims the host reads out of a directory,
// so the two fences apply to both kinds — a backup job still writing is
// the operator's to wait for, and a backup of several nodes is not a
// thing a single-node sandbox can prove.
func TestAnArchiveStatingAnIncompleteBackupIsRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		meta    string
		code    string
		message string
	}{
		"a backup that never finished": {
			`{"id":"nightly","status":"TRANSFERRING","nodes":{"node1":{"classes":["Orders"],"status":"SUCCESS"}}}`,
			"source_unreadable", "still writing it",
		},
		"a backup of several nodes": {
			`{"id":"nightly","status":"SUCCESS","nodes":{"node1":{"classes":["Orders"],"status":"SUCCESS"},` +
				`"node2":{"classes":["Orders"],"status":"SUCCESS"}}}`,
			"invalid_request", "taken on 2 nodes",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, perr := readArchiveMeta(context.Background(), answeredCore(t, said(0, tc.meta, "")),
				&resolvedSource{path: "/backups/nightly.tar", tarball: true}, "/work/extract")
			wantRefusal(t, perr, tc.code, tc.message)
		})
	}
}

func TestTransferTreeRecreatesTheBackupFileByFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "Orders"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"backup_config.json", "Orders/chunk-0"} {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), []byte("bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	put := putFileValue{BytesCopied: 5, DurationSeconds: 0.25}
	seconds, perr := transferTree(context.Background(),
		answeredCore(t, said(0, "", ""), put, put), dir, "/backups/nightly")
	if perr != nil || seconds != 0.5 {
		t.Errorf("transferTree = %v, %+v, want the two transfers it measured", seconds, perr)
	}
	if _, perr := transferTree(context.Background(), answeredCore(t),
		filepath.Join(dir, "gone"), "/backups/nightly"); perr == nil || perr.Code != "source_unreadable" {
		t.Errorf("perr = %+v, want source_unreadable for a tree the host cannot walk", perr)
	}
}

// TestTheEngineIsTheJudgeOfItsOwnArtifact: the node starts on an empty
// data directory, so a start that fails is the drill's environment; a
// restore the engine refuses is about the backup, in the engine's own
// words.
func TestTheEngineIsTheJudgeOfItsOwnArtifact(t *testing.T) {
	t.Run("a node that would not start", func(t *testing.T) {
		_, perr := startEngine(context.Background(),
			answeredCore(t, said(1, "", "weaviate did not answer within 120s\n")), "node1", "/backups", "/data")
		wantRefusal(t, perr, "restore_failed", "did not start")
	})
	t.Run("a node that came up", func(t *testing.T) {
		seconds, perr := startEngine(context.Background(), answeredCore(t, said(0, "", "")), "node1", "/backups", "/data")
		if perr != nil || seconds != 0.25 {
			t.Errorf("startEngine = %v, %+v, want the measured start", seconds, perr)
		}
	})
	t.Run("a restore the engine refused", func(t *testing.T) {
		_, perr := restoreBackup(context.Background(),
			answeredCore(t, said(1, "", `"error":"cannot read chunk: unexpected EOF"`+"\n")), "nightly")
		wantRefusal(t, perr, "source_corrupt", "unexpected EOF")
	})
	t.Run("a restore that finished", func(t *testing.T) {
		seconds, perr := restoreBackup(context.Background(), answeredCore(t, said(0, "", "")), "nightly")
		if perr != nil || seconds != 0.25 {
			t.Errorf("restoreBackup = %v, %+v, want the measured restore", seconds, perr)
		}
	})
}

// TestAssertRestoredReadsTheClassesOwnCount: an empty class has a valid
// backup, so a restore that proves nothing looks exactly like one that
// worked — the object count is what tells them apart.
func TestAssertRestoredReadsTheClassesOwnCount(t *testing.T) {
	for name, tc := range map[string]struct {
		answers []any
		count   string
		code    string
		message string
	}{
		"objects restored": {[]any{said(0, "2200\n", "")}, "2200", "", ""},
		"a class holding nothing": {
			[]any{said(0, "0\n", "")}, "", "source_corrupt", "holds no objects",
		},
		"a class that does not answer": {
			[]any{said(7, "", "wget: server returned error 500\n")}, "", "restore_failed", "does not answer",
		},
		"a class that answered nothing": {
			[]any{said(0, "  \n", "")}, "", "restore_failed", "did not report how many objects",
		},
		"a core that went away mid-call": {nil, "", "internal", "closed the stream"},
	} {
		t.Run(name, func(t *testing.T) {
			count, perr := assertRestored(context.Background(), answeredCore(t, tc.answers...), "Orders")
			if tc.code == "" {
				if perr != nil || count != tc.count {
					t.Errorf("assertRestored = %q, %+v, want %q", count, perr, tc.count)
				}
				return
			}
			wantRefusal(t, perr, tc.code, tc.message)
		})
	}
}

// TestTheStatePayloadFallsBackOnlyWhereTheBackupIsSilent: the id, the node
// and the instant come from the backup's own metadata where it states
// them, and the record says nothing rather than something invented where
// it does not.
func TestTheStatePayloadFallsBackOnlyWhereTheBackupIsSilent(t *testing.T) {
	instant := "2026-09-17T18:00:00.000Z"
	stated := &resolvedSource{
		meta:      &backupMeta{ID: "nightly"},
		node:      "node1",
		createdAt: &instant,
	}
	if backupID(stated) != "nightly" || nodeName(stated) != "node1" || createdAtValue(stated) != instant {
		t.Errorf("stated = %v / %v / %v, want the backup's own claims",
			backupID(stated), nodeName(stated), createdAtValue(stated))
	}
	silent := &resolvedSource{}
	if backupID(silent) != fallbackID || nodeName(silent) != fallbackNode || createdAtValue(silent) != nil {
		t.Errorf("silent = %v / %v / %v, want the fallbacks and no instant",
			backupID(silent), nodeName(silent), createdAtValue(silent))
	}
}

// TestFirstLineStaysProtocolSafe: what the sandbox printed lands in a JSON
// string and in evidence error fields, so it crosses as one line without
// quotes of its own.
func TestFirstLineStaysProtocolSafe(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"the first line of several": {"  wget: server returned 500\nand more\n", "wget: server returned 500"},
		"quotes the engine wrote":   {`{"error":"not found"}`, `{'error':'not found'}`},
		"nothing at all":            {"\n  \n", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := firstLine([]byte(tc.in)); got != tc.want {
				t.Errorf("firstLine = %q, want %q", got, tc.want)
			}
		})
	}
}
