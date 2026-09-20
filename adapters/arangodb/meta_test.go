package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheReadinessLoopWaitsRatherThanGivingUp: the server answers when it
// answers, and a first refusal is not a failed drill.
func TestTheReadinessLoopWaitsRatherThanGivingUp(t *testing.T) {
	dir := t.TempDir()
	writeDump(t, dir, oneDump())
	var sequence []string
	base := provisionHandler(t, &sequence, defaultSimulated())
	refusals := 2
	line, _, _ := driveOp(t, "provision",
		provisionPayload(t, "arangodb_dump", dir, nil),
		func(call verbCall) (any, *protoError) {
			if call.Verb == "exec" {
				argv := argvOf(t, call)
				if argv[0] == "arangosh" && !strings.Contains(strings.Join(argv, " "), "_collections") &&
					refusals > 0 {
					refusals--
					sequence = append(sequence, "not-ready")
					return errExec(1, "not connected"), nil
				}
			}
			return base(call)
		})
	if f := parseFinal(t, line); !f.OK {
		t.Fatalf("provision failed while the server was still coming up: %+v", f.Error)
	}
	if refusals != 0 {
		t.Errorf("the loop gave up after %d refusals", 2-refusals)
	}
	if !contains(sequence, "not-ready") || !contains(sequence, "ready") {
		t.Errorf("the flow did not wait and then proceed: %v", sequence)
	}
}

// TestAWorkDirectoryTheSandboxRefusesIsInternal, rather than blamed on
// the backup.
func TestAWorkDirectoryTheSandboxRefusesIsInternal(t *testing.T) {
	dir := t.TempDir()
	writeDump(t, dir, oneDump())
	var sequence []string
	base := provisionHandler(t, &sequence, defaultSimulated())
	line, _, _ := driveOp(t, "provision",
		provisionPayload(t, "arangodb_dump", dir, nil),
		func(call verbCall) (any, *protoError) {
			if call.Verb == "exec" && argvOf(t, call)[0] == "mkdir" {
				return errExec(1, "read-only file system"), nil
			}
			return base(call)
		})
	wantCode(t, parseFinal(t, line), "internal", "prepare work directory")
}

// TestTheCheckRunnerIsWrittenBeforeTheEngineStarts: a check cannot run
// without it, and it is written with a here-document rather than
// put_file, because put_file may only carry paths belonging to the
// drill's backup source (§4.2).
func TestTheCheckRunnerIsWrittenBeforeTheEngineStarts(t *testing.T) {
	dir := t.TempDir()
	writeDump(t, dir, oneDump())
	var scripts []string
	line, _, _ := driveOp(t, "provision",
		provisionPayload(t, "arangodb_dump", dir, nil),
		recordScripts(t, &scripts))
	if f := parseFinal(t, line); !f.OK {
		t.Fatalf("provision failed: %+v", f.Error)
	}
	wrote := indexOfScript(scripts, func(s string) bool { return strings.HasPrefix(s, "cat > ") })
	started := indexOfScript(scripts, func(s string) bool { return strings.Contains(s, "nohup") })
	if wrote < 0 || started < 0 || wrote > started {
		t.Errorf("runner written at %d, engine started at %d — the runner must exist first",
			wrote, started)
	}
	if wrote >= 0 && !strings.Contains(scripts[wrote], runnerFile) {
		t.Errorf("the runner went to %q, not the path the probe declares", scripts[wrote])
	}
}

// recordScripts drives the happy path while keeping every shell script
// the adapter ran, and fails if the runner is sent with put_file — which
// may only carry paths belonging to the drill's backup source (§4.2).
func recordScripts(t *testing.T, scripts *[]string) func(verbCall) (any, *protoError) {
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			args := struct {
				DestPath string `json:"dest_path"`
			}{}
			if err := json.Unmarshal(call.Args, &args); err == nil &&
				strings.Contains(args.DestPath, "runner") {
				t.Errorf("the runner was sent with put_file to %s", args.DestPath)
			}
			return putFileValue{}, nil
		}
		argv := argvOf(t, call)
		if argv[0] == "sh" {
			*scripts = append(*scripts, argv[2])
		}
		label, value := classifyExec(argv, defaultSimulated())
		if label == "" {
			t.Fatalf("unexpected exec: %v", argv)
		}
		return value, nil
	}
}

func indexOfScript(scripts []string, match func(string) bool) int {
	for i, s := range scripts {
		if match(s) {
			return i
		}
	}
	return -1
}

// TestAnArchiveCannotChooseHowMuchMemoryTheHostSpends: a tar entry is a
// 512-byte header that compresses to almost nothing, and a backup file is
// attacker-controlled input.
func TestAnArchiveCannotChooseHowMuchMemoryTheHostSpends(t *testing.T) {
	buf := &bytes.Buffer{}
	tw := tar.NewWriter(buf)
	for i := 0; i <= keptMaxEntries; i++ {
		if err := tw.WriteHeader(&tar.Header{
			Name: "pad/f", Typeflag: tar.TypeReg, Size: 0,
		}); err != nil {
			t.Fatalf("pack: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	_, verdict, ok := walkTar(tar.NewReader(bytes.NewReader(buf.Bytes())))
	if !ok || verdict == nil || verdict.Code != "source_corrupt" {
		t.Fatalf("verdict = %+v (ok=%v), want a source_corrupt refusal", verdict, ok)
	}
	if !strings.Contains(verdict.Message, "exceeds what an arangodump output carries") {
		t.Errorf("message = %q", verdict.Message)
	}
}

// TestAnArchiveWithNoFilesSaysNothing: an empty stream is not a verdict.
func TestAnArchiveWithNoFilesSaysNothing(t *testing.T) {
	buf := &bytes.Buffer{}
	tw := tar.NewWriter(buf)
	if err := tw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	census, verdict, ok := walkTar(tar.NewReader(bytes.NewReader(buf.Bytes())))
	if ok || verdict != nil || census.database != "" {
		t.Errorf("an empty archive produced a claim: ok=%v verdict=%+v census=%+v", ok, verdict, census)
	}
}

func TestReadCappedStopsAtTheCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.json")
	writeFile(t, path, strings.Repeat("x", metaMaxBytes+4096))
	raw, err := readCapped(path)
	if err != nil {
		t.Fatalf("readCapped: %v", err)
	}
	if len(raw) != metaMaxBytes {
		t.Errorf("read %d bytes, want the cap of %d", len(raw), metaMaxBytes)
	}
	if _, err := readCapped(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("a missing file read without error")
	}
}

// TestTheMessageNamesAFewCollectionsNotAll keeps a refusal readable; the
// drill host's log has the rest.
func TestTheMessageNamesAFewCollectionsNotAll(t *testing.T) {
	dir := t.TempDir()
	f := dumpFixture{
		database: "shop", createdAt: "2026-09-20T14:21:39Z",
		collections: []string{"alpha", "bravo", "charlie", "delta", "echo"},
	}
	writeDump(t, dir, f)
	// Remove every data file, so all five are missing their half.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".data.json") {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				t.Fatalf("remove: %v", err)
			}
		}
	}
	_, perr := resolveSource("arangodb_dump", dir)
	if perr == nil {
		t.Fatal("a dump with no data at all was accepted")
	}
	cut := strings.Index(perr.Message, " has its definition")
	if cut < 0 {
		t.Fatalf("message does not name the collections at all: %q", perr.Message)
	}
	named := perr.Message[:cut]
	for _, want := range []string{"alpha", "bravo", "charlie"} {
		if !strings.Contains(named, want) {
			t.Errorf("the message does not name %q: %q", want, named)
		}
	}
	for _, unwanted := range []string{"delta", "echo"} {
		if strings.Contains(named, unwanted) {
			t.Errorf("the message names %q too; a refusal lists a few, not all: %q", unwanted, named)
		}
	}
}

func TestTrimListKeepsAtMostThree(t *testing.T) {
	if got := trimList([]string{"a", "b"}); len(got) != 2 {
		t.Errorf("trimList kept %d of 2", len(got))
	}
	if got := trimList([]string{"a", "b", "c", "d"}); len(got) != 3 {
		t.Errorf("trimList kept %d of 4, want 3", len(got))
	}
}
