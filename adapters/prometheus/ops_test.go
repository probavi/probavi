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

func TestParseInstantCount(t *testing.T) {
	tests := []struct {
		name   string
		line   string
		want   int64
		wantOK bool
	}{
		{"the measured shape", "{} => 680 @[1786876374]", 680, true},
		{"zero", "{} => 0 @[1786876374]", 0, true},
		{"a labeled vector", `up{job='self'} => 1 @[1786876374.046]`, 1, true},
		{"an empty answer", "", 0, false},
		{"the simulated sandbox's stdout", "1", 0, false},
		{"a float value is not this count", "{} => 1.5 @[1]", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseInstantCount(tt.line)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("parseInstantCount(%q) = %d/%v, want %d/%v", tt.line, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestLastErrorLine(t *testing.T) {
	log := []byte(`time=2026-08-16T10:29:51.000Z level=INFO msg="Starting TSDB ..."
time=2026-08-16T10:29:51.383Z level=ERROR source=main.go:1618 msg="Fatal error" err="opening storage failed: reloadBlocks: corrupted block 01M051XQ: read symbols: invalid checksum"`)
	got := lastErrorLine(log)
	if !strings.Contains(got, "corrupted block") {
		t.Errorf("lastErrorLine = %q, want the server's own failure report", got)
	}
	if strings.Contains(got, `"`) {
		t.Errorf("lastErrorLine = %q must stay quote-free for protocol embedding", got)
	}
	if lastErrorLine([]byte("level=INFO msg=ready\n")) != "" {
		t.Error("a healthy log must yield nothing")
	}
}

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

// TestCheckEngineNamesTheImageProblem: the official images pin the server
// as their entrypoint and cannot idle as a drill sandbox, so the refusal
// points at the wrapper image rather than at the backup.
func TestCheckEngineNamesTheImageProblem(t *testing.T) {
	if perr := checkEngine(context.Background(), answeredCore(t, said(0, "prometheus, version 3.13.0", ""))); perr != nil {
		t.Errorf("perr = %+v, want a usable image to pass", perr)
	}
	perr := checkEngine(context.Background(), answeredCore(t, said(127, "", "sh: prometheus: not found\n")))
	if perr == nil || perr.Code != "invalid_request" || !strings.Contains(perr.Message, "not found") {
		t.Errorf("perr = %+v, want invalid_request carrying the image's own complaint", perr)
	}
	if perr := checkEngine(context.Background(), answeredCore(t)); perr == nil || perr.Code != "internal" {
		t.Errorf("perr = %+v, want the harness's own failure", perr)
	}
}

func TestMkdirAllReportsWhatTheShellSaid(t *testing.T) {
	if perr := mkdirAll(context.Background(), answeredCore(t, said(0, "", "")), "/work/data"); perr != nil {
		t.Errorf("perr = %+v, want a made directory to pass", perr)
	}
	perr := mkdirAll(context.Background(),
		answeredCore(t, said(1, "", "mkdir: can't create directory '/work': Read-only file system\n")), "/work/data")
	if perr == nil || perr.Code != "internal" || !strings.Contains(perr.Message, "Read-only file system") {
		t.Errorf("perr = %+v, want internal carrying the shell's words", perr)
	}
}

// TestUnpackArchiveFindsTheBlocksWhereverTheArchiveNestedThem: a snapshot
// tars either the blocks at its root or one wrapping directory above
// them, so the sandbox is asked where they landed and the answer is what
// the server is pointed at — with the extraction directory as the
// fallback when the question could not be answered.
func TestUnpackArchiveFindsTheBlocksWhereverTheArchiveNestedThem(t *testing.T) {
	const workDir = "/work"
	put := putFileValue{BytesCopied: 4096, DurationSeconds: 0.5}
	for name, tc := range map[string]struct {
		answers  []any
		wantDir  string
		code     string
		message  string
		transfer float64
	}{
		"blocks one level down": {
			[]any{said(0, "", ""), put, said(0, "", ""), said(0, workDir+"/extract/snapshot\n", "")},
			workDir + "/extract/snapshot", "", "", 0.5,
		},
		"an answer the sandbox could not give": {
			[]any{said(0, "", ""), put, said(0, "", ""), said(1, "", "sh: bad substitution\n")},
			workDir + "/extract", "", "", 0.5,
		},
		"an archive tar refused": {
			[]any{said(0, "", ""), put, said(2, "", "tar: invalid tar magic\n")},
			"", "source_corrupt", "invalid tar magic", 0,
		},
		"a work directory that could not be made": {
			[]any{said(1, "", "mkdir: permission denied\n")}, "", "internal", "prepare work directory", 0,
		},
		"a core that went away mid-call": {nil, "", "internal", "closed the stream", 0},
	} {
		t.Run(name, func(t *testing.T) {
			transfer, unpack, dataDir, perr := unpackArchive(context.Background(),
				answeredCore(t, tc.answers...), "/backups/snapshot.tar", workDir)
			if tc.code != "" {
				if perr == nil || perr.Code != tc.code || !strings.Contains(perr.Message, tc.message) {
					t.Fatalf("perr = %+v, want %s mentioning %q", perr, tc.code, tc.message)
				}
				return
			}
			if perr != nil {
				t.Fatalf("perr = %+v", perr)
			}
			if dataDir != tc.wantDir || transfer != tc.transfer || unpack <= 0 {
				t.Errorf("unpackArchive = %v, %v, %q, want %q served after a measured unpack",
					transfer, unpack, dataDir, tc.wantDir)
			}
		})
	}
}

// TestTransferTreeRecreatesTheSnapshotFileByFile: a directory artifact
// moves as its own files, so the skeleton is made first and every regular
// file follows it — and a tree the host cannot walk is its failure to
// report, not the sandbox's.
func TestTransferTreeRecreatesTheSnapshotFileByFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "01BLOCK", "chunks"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"01BLOCK/meta.json", "01BLOCK/chunks/000001"} {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), []byte("bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	put := putFileValue{BytesCopied: 5, DurationSeconds: 0.25}
	seconds, perr := transferTree(context.Background(),
		answeredCore(t, said(0, "", ""), put, put), dir, "/work/data")
	if perr != nil {
		t.Fatalf("perr = %+v", perr)
	}
	if seconds != 0.5 {
		t.Errorf("transferTree = %v, want the two transfers it measured", seconds)
	}
	_, perr = transferTree(context.Background(), answeredCore(t), filepath.Join(dir, "gone"), "/work/data")
	if perr == nil || perr.Code != "source_unreadable" || !strings.Contains(perr.Message, "read backup directory") {
		t.Errorf("perr = %+v, want source_unreadable", perr)
	}
}

// TestStartEngineCarriesTheServersOwnReason: a TSDB that will not open
// says why in its log, and that line decides whose fault the drill's
// failure was — the backup's when it names corruption, the restore's
// otherwise.
func TestStartEngineCarriesTheServersOwnReason(t *testing.T) {
	const corrupt = `ts=2026-09-17T18:00:00.000Z caller=main.go:1 level=error err="opening storage failed: ` +
		`corrupted block 01BLOCK: invalid checksum"`
	for name, tc := range map[string]struct {
		answers []any
		code    string
		message string
	}{
		"a server that came up": {
			[]any{said(0, "", ""), said(0, "", ""), said(0, "", "")}, "", "",
		},
		"a config the sandbox would not write": {
			[]any{said(1, "", "sh: cannot create /work/prometheus.yml\n")}, "internal", "write server config",
		},
		"a server that would not launch": {
			[]any{said(0, "", ""), said(1, "", "sh: prometheus: not found\n")},
			"restore_failed", "failed to launch",
		},
		"a TSDB the backup corrupted": {
			[]any{said(0, "", ""), said(0, "", ""), said(1, "", ""), said(0, "", ""), said(0, corrupt, "")},
			"source_corrupt", "invalid checksum",
		},
		"a server that died for another reason": {
			[]any{said(0, "", ""), said(0, "", ""), said(1, "", ""), said(0, "", ""),
				said(0, `ts=2026-09-17T18:00:00.000Z level=error err="listen tcp: address already in use"`, "")},
			"restore_failed", "address already in use",
		},
		"a log that says nothing": {
			[]any{said(0, "", ""), said(0, "", ""), said(1, "", ""), said(0, "", ""), said(0, "starting\n", "")},
			"engine_not_ready", "exited during startup",
		},
	} {
		t.Run(name, func(t *testing.T) {
			seconds, perr := startEngine(context.Background(), answeredCore(t, tc.answers...), "/work", "/work/data")
			if tc.code == "" {
				if perr != nil || seconds < 0.25 {
					t.Errorf("startEngine = %v, %+v, want the wait it measured", seconds, perr)
				}
				return
			}
			if perr == nil || perr.Code != tc.code || !strings.Contains(perr.Message, tc.message) {
				t.Errorf("perr = %+v, want %s mentioning %q", perr, tc.code, tc.message)
			}
		})
	}
}

// TestCheckCensusRefusesOnPositiveEvidenceOnly: the restored server's own
// metric decides, and only when it reads as one. Anything else leaves the
// verdict to the checks that follow.
func TestCheckCensusRefusesOnPositiveEvidenceOnly(t *testing.T) {
	for name, tc := range map[string]struct {
		answers []any
		census  snapshotInfo
		code    string
		message string
	}{
		"every block loaded": {
			[]any{said(0, "prometheus_tsdb_blocks_loaded 3\n", "")}, snapshotInfo{blocks: 3}, "", "",
		},
		"a block that did not load": {
			[]any{said(0, "prometheus_tsdb_blocks_loaded 2\n", "")}, snapshotInfo{blocks: 3, sourcesSkipped: 1},
			"source_corrupt", "loaded 2 of the 3 blocks",
		},
		"a server holding nothing at all": {
			[]any{said(0, "prometheus_tsdb_blocks_loaded 0\n", "")}, snapshotInfo{},
			"source_corrupt", "no blocks at all",
		},
		"a metric the server did not serve": {
			[]any{said(1, "", "wget: server returned error\n")}, snapshotInfo{blocks: 3}, "", "",
		},
		"a line that is not the metric": {
			[]any{said(0, "# HELP prometheus_tsdb_blocks_loaded blocks\n", "")}, snapshotInfo{blocks: 3}, "", "",
		},
		"a count that is not a number": {
			[]any{said(0, "prometheus_tsdb_blocks_loaded many\n", "")}, snapshotInfo{blocks: 3}, "", "",
		},
		"a core that went away mid-call": {nil, snapshotInfo{blocks: 3}, "internal", "closed the stream"},
	} {
		t.Run(name, func(t *testing.T) {
			_, perr := checkCensus(context.Background(), answeredCore(t, tc.answers...), tc.census)
			if tc.code == "" {
				if perr != nil {
					t.Errorf("perr = %+v, want the verdict left to the checks", perr)
				}
				return
			}
			if perr == nil || perr.Code != tc.code || !strings.Contains(perr.Message, tc.message) {
				t.Errorf("perr = %+v, want %s mentioning %q", perr, tc.code, tc.message)
			}
		})
	}
}
