package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
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

// TestCheckSandboxNamesWhatTheImageLacks: the snapshot flags are startup
// arguments, so a sandbox whose image already started Qdrant could not be
// restored into at all — and the refusal carries the sandbox's own words
// about which of the three requirements was missing.
func TestCheckSandboxNamesWhatTheImageLacks(t *testing.T) {
	if perr := checkSandbox(context.Background(), answeredCore(t, said(0, "", ""))); perr != nil {
		t.Errorf("perr = %+v, want an idle qdrant image to pass", perr)
	}
	for name, stderr := range map[string]string{
		"no bash":            "no bash in the sandbox image\n",
		"not a qdrant image": "no /qdrant/qdrant — this is not a qdrant image\n",
		"already serving":    "qdrant is already serving on 6333 — the sandbox must start idle\n",
	} {
		t.Run(name, func(t *testing.T) {
			perr := checkSandbox(context.Background(), answeredCore(t, said(1, "", stderr)))
			if perr == nil || perr.Code != "invalid_request" ||
				!strings.Contains(perr.Message, strings.TrimSpace(strings.SplitN(stderr, "\n", 2)[0])) {
				t.Errorf("perr = %+v, want invalid_request carrying %q", perr, stderr)
			}
		})
	}
	if perr := checkSandbox(context.Background(), answeredCore(t)); perr == nil || perr.Code != "internal" {
		t.Errorf("perr = %+v, want the harness's own failure", perr)
	}
}

func TestPrepareWorkDirReportsWhatTheShellSaid(t *testing.T) {
	if perr := prepareWorkDir(context.Background(), answeredCore(t, said(0, "", "")), "/work"); perr != nil {
		t.Errorf("perr = %+v, want a made directory to pass", perr)
	}
	perr := prepareWorkDir(context.Background(),
		answeredCore(t, said(1, "", "mkdir: can't create directory '/work': Read-only file system\n")), "/work")
	if perr == nil || perr.Code != "internal" || !strings.Contains(perr.Message, "Read-only file system") {
		t.Errorf("perr = %+v, want internal carrying the shell's words", perr)
	}
	if perr := prepareWorkDir(context.Background(), answeredCore(t), "/work"); perr == nil {
		t.Error("a core that went away was taken as a made directory")
	}
}

// TestAssertRestoredReadsTheCollectionsOwnCount: an empty collection has a
// valid snapshot, so a restore that proves nothing looks exactly like one
// that worked — the point count is what tells them apart.
func TestAssertRestoredReadsTheCollectionsOwnCount(t *testing.T) {
	for name, tc := range map[string]struct {
		answers []any
		count   string
		code    string
		message string
	}{
		"points restored": {[]any{said(0, "1200\n", "")}, "1200", "", ""},
		"a collection of no points": {
			[]any{said(0, "0\n", "")}, "", "source_corrupt", "holds no points",
		},
		"a collection that does not answer": {
			[]any{said(7, "", "bash: connection refused\n")}, "", "restore_failed", "does not answer",
		},
		"a collection that answered nothing": {
			[]any{said(0, "  \n", "")}, "", "restore_failed", "did not report how many points",
		},
		"a core that went away mid-call": {nil, "", "internal", "closed the stream"},
	} {
		t.Run(name, func(t *testing.T) {
			count, perr := assertRestored(context.Background(), answeredCore(t, tc.answers...), "drill")
			if tc.code == "" {
				if perr != nil || count != tc.count {
					t.Errorf("assertRestored = %q, %+v, want %q", count, perr, tc.count)
				}
				return
			}
			if perr == nil || perr.Code != tc.code || !strings.Contains(perr.Message, tc.message) {
				t.Errorf("perr = %+v, want %s mentioning %q", perr, tc.code, tc.message)
			}
		})
	}
}

// TestHealthcheckAnswersWithTheCollectionsState: an unhealthy collection
// is a result rather than an operation error, and a payload naming no
// collection is the core's mistake.
func TestHealthcheckAnswersWithTheCollectionsState(t *testing.T) {
	t.Run("a payload naming no collection", func(t *testing.T) {
		line, calls, exit := driveOp(t, "healthcheck", `{"connection":{}}`, func(verbCall) (any, *protoError) {
			t.Error("the sandbox was touched for a payload that names no collection")
			return said(0, "", ""), nil
		})
		if exit != 0 || len(calls) != 0 {
			t.Fatalf("exit = %d, sandbox calls = %d", exit, len(calls))
		}
		f := parseFinal(t, line)
		if f.OK || f.Error == nil || f.Error.Code != "invalid_request" {
			t.Errorf("final = %+v, want invalid_request", f)
		}
	})
	t.Run("a payload that is not one", func(t *testing.T) {
		line, calls, exit := driveOp(t, "healthcheck", `"not an object"`, func(verbCall) (any, *protoError) {
			t.Error("the sandbox was touched for a payload that could not be read")
			return said(0, "", ""), nil
		})
		if exit != 0 || len(calls) != 0 {
			t.Fatalf("exit = %d, sandbox calls = %d", exit, len(calls))
		}
		f := parseFinal(t, line)
		if f.OK || f.Error == nil || f.Error.Code != "invalid_request" {
			t.Errorf("final = %+v, want invalid_request", f)
		}
	})
}

// TestFirstLineStaysProtocolSafe: what the sandbox printed lands in a JSON
// string and in evidence error fields, so it crosses as one line without
// quotes of its own.
func TestFirstLineStaysProtocolSafe(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"the first line of several": {"  bash: connection refused\nand more\n", "bash: connection refused"},
		"quotes the engine wrote":   {`{"status":"not found"}`, `{'status':'not found'}`},
		"nothing at all":            {"\n  \n", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := firstLine([]byte(tc.in)); got != tc.want {
				t.Errorf("firstLine = %q, want %q", got, tc.want)
			}
		})
	}
}
