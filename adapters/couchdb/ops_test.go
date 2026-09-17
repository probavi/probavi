package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
)

// answeredCore is a core whose next sandbox calls are answered as given,
// so a step can be driven straight to the branch under test. A core built
// with no answers is one whose stream is gone: the step that calls it then
// gets the harness's own failure, which is what a step meets when the core
// went away mid-operation.
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

// testLogger writes the adapter's own log lines nowhere: a step's
// logging is not what a test about its verdict is reading.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
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

func TestOptionFallsBackOnlyWhenNothingWasSaid(t *testing.T) {
	options := map[string]string{"user": "operator", "database": "   ", "empty": ""}
	for name, tc := range map[string]struct{ key, fallback, want string }{
		"a value the drill gave":  {"user", "admin", "operator"},
		"a value of only spaces":  {"database", "drill", "drill"},
		"a value that is empty":   {"empty", "drill", "drill"},
		"a key the drill omitted": {"absent", "drill", "drill"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := option(options, tc.key, tc.fallback); got != tc.want {
				t.Errorf("option(%q) = %q, want %q", tc.key, got, tc.want)
			}
		})
	}
}

// TestCheckSandboxNamesWhatTheImageLacks: this adapter owns the engine's
// whole lifetime, so a sandbox that is not the idle CouchDB image it needs
// is refused with the image's own complaint rather than in the middle of a
// restore.
func TestCheckSandboxNamesWhatTheImageLacks(t *testing.T) {
	t.Run("the sandbox this adapter needs", func(t *testing.T) {
		seconds, perr := checkSandbox(context.Background(), answeredCore(t, said(0, "", "")))
		if perr != nil || seconds != 0.25 {
			t.Errorf("checkSandbox = %v, %+v, want the measured duration", seconds, perr)
		}
	})
	t.Run("an image without curl", func(t *testing.T) {
		_, perr := checkSandbox(context.Background(), answeredCore(t, said(1, "", "no curl in the sandbox image\n")))
		if perr == nil || perr.Code != "invalid_request" || !strings.Contains(perr.Message, "no curl") {
			t.Errorf("perr = %+v, want invalid_request carrying the sandbox's words", perr)
		}
	})
	t.Run("a core that went away mid-call", func(t *testing.T) {
		if _, perr := checkSandbox(context.Background(), answeredCore(t)); perr == nil || perr.Code != "internal" {
			t.Errorf("perr = %+v, want the harness's own failure", perr)
		}
	})
}

func TestPrepareWorkDirReportsWhatTheShellSaid(t *testing.T) {
	if perr := prepareWorkDir(context.Background(), answeredCore(t, said(0, "", "")), "/tmp/probavi"); perr != nil {
		t.Errorf("perr = %+v, want a made directory to pass", perr)
	}
	perr := prepareWorkDir(context.Background(),
		answeredCore(t, said(1, "", "mkdir: cannot create directory: Read-only file system\n")), "/tmp/probavi")
	if perr == nil || perr.Code != "internal" || !strings.Contains(perr.Message, "Read-only file system") {
		t.Errorf("perr = %+v, want internal carrying the shell's words", perr)
	}
	if perr := prepareWorkDir(context.Background(), answeredCore(t), "/tmp/probavi"); perr == nil {
		t.Error("a core that went away was taken as a made directory")
	}
}

// TestAssertRestoredReadsTheDatabasesOwnCount: the verdict is the
// database's document count, not that the engine answered — a truncated
// artifact opens as the smaller database its remaining bytes describe, so
// "the server started" proves nothing.
func TestAssertRestoredReadsTheDatabasesOwnCount(t *testing.T) {
	for name, tc := range map[string]struct {
		answers []any
		code    string
		message string
	}{
		"documents restored": {[]any{said(0, "500\n", "")}, "", ""},
		"a database serving none": {
			[]any{said(0, "0\n", "")}, "source_corrupt", "holds no documents",
		},
		"a database that does not answer": {
			[]any{said(7, "", "curl: (7) Failed to connect\n")}, "restore_failed", "does not answer",
		},
		"a database that answered nothing": {
			[]any{said(0, "   \n", "")}, "restore_failed", "did not report how many documents",
		},
		"a core that went away mid-call": {nil, "internal", "closed the stream"},
	} {
		t.Run(name, func(t *testing.T) {
			perr := assertRestored(context.Background(), answeredCore(t, tc.answers...), "admin", "drill")
			if tc.code == "" {
				if perr != nil {
					t.Errorf("perr = %+v, want the restore to stand", perr)
				}
				return
			}
			if perr == nil || perr.Code != tc.code || !strings.Contains(perr.Message, tc.message) {
				t.Errorf("perr = %+v, want %s mentioning %q", perr, tc.code, tc.message)
			}
		})
	}
}

// TestRestoreStepsReportWhoseFaultTheFailureWas: a replay the engine
// refused is a failed restore, and a data directory that would not go into
// place is a statement about the artifact.
func TestRestoreStepsReportWhoseFaultTheFailureWas(t *testing.T) {
	t.Run("a batch the engine refused", func(t *testing.T) {
		core := answeredCore(t,
			said(0, "", ""), // start the engine
			putFileValue{BytesCopied: 64, DurationSeconds: 0.5}, // transfer
			said(1, "", "batch 2 refused with 500\n"),           // replay
		)
		_, _, perr := restoreBackup(context.Background(), core,
			&resolvedSource{path: "/backups/nightly.txt", form: formBackup, batches: 3}, "/tmp/probavi", "admin", "drill")
		if perr == nil || perr.Code != "restore_failed" || !strings.Contains(perr.Message, "batch 2 refused") {
			t.Errorf("perr = %+v, want restore_failed carrying the engine's words", perr)
		}
	})
	t.Run("a data directory that would not go into place", func(t *testing.T) {
		core := answeredCore(t,
			putFileValue{BytesCopied: 4096, DurationSeconds: 0.75},
			said(1, "", "no _dbs.couch after placing the data\n"),
		)
		_, _, perr := restoreDataDir(context.Background(), core,
			&resolvedSource{path: "/backups/data.tar", form: formDataTar}, "/tmp/probavi", "admin", "drill", testLogger())
		if perr == nil || perr.Code != "source_corrupt" || !strings.Contains(perr.Message, "_dbs.couch") {
			t.Errorf("perr = %+v, want source_corrupt naming what the sandbox missed", perr)
		}
	})
	t.Run("a core that went away before the transfer", func(t *testing.T) {
		_, _, perr := restoreDataDir(context.Background(), answeredCore(t),
			&resolvedSource{path: "/backups/data", form: formDataDir}, "/tmp/probavi", "admin", "drill", testLogger())
		if perr == nil || perr.Code != "internal" {
			t.Errorf("perr = %+v, want the harness's own failure", perr)
		}
	})
}

// TestHealthcheckAnswersWithTheDatabasesState: an unhealthy database is a
// result rather than an operation error, and a payload naming no database
// is the core's mistake.
func TestHealthcheckAnswersWithTheDatabasesState(t *testing.T) {
	t.Run("a payload naming no database", func(t *testing.T) {
		line, calls, exit := driveOp(t, "healthcheck", `{"connection":{}}`, func(verbCall) (any, *protoError) {
			t.Error("the sandbox was touched for a payload that names no database")
			return okExec(), nil
		})
		if exit != 0 || len(calls) != 0 {
			t.Fatalf("exit = %d, sandbox calls = %d", exit, len(calls))
		}
		f := parseFinal(t, line)
		if f.OK || f.Error == nil || f.Error.Code != "invalid_request" {
			t.Errorf("final = %+v, want invalid_request", f)
		}
	})
	for name, tc := range map[string]struct {
		answer  any
		healthy bool
		detail  string
	}{
		"a database that serves":      {said(0, "500\n", ""), true, "500 documents"},
		"a database that does not":    {said(7, "", "curl: (7) Failed to connect\n"), false, "exited 7: curl: (7)"},
		"an engine that said nothing": {said(22, "", ""), false, "exited 22"},
	} {
		t.Run(name, func(t *testing.T) {
			line, _, exit := driveOp(t, "healthcheck",
				`{"connection":{"database":"drill","user":"admin"},"state":{}}`,
				func(verbCall) (any, *protoError) { return tc.answer, nil })
			if exit != 0 {
				t.Fatalf("exit = %d", exit)
			}
			f := parseFinal(t, line)
			if !f.OK {
				t.Fatalf("final = %+v", f)
			}
			got := struct {
				Healthy bool   `json:"healthy"`
				Detail  string `json:"detail"`
			}{}
			if err := json.Unmarshal(f.Payload, &got); err != nil {
				t.Fatal(err)
			}
			if got.Healthy != tc.healthy || !strings.Contains(got.Detail, tc.detail) {
				t.Errorf("healthy = %v, detail = %q, want %v mentioning %q",
					got.Healthy, got.Detail, tc.healthy, tc.detail)
			}
		})
	}
}

// TestFirstLineStaysProtocolSafe: what the sandbox printed lands in a JSON
// string and in evidence error fields, so it crosses as one line without
// quotes of its own.
func TestFirstLineStaysProtocolSafe(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"the first line of several": {"  curl: (7) refused\nand more\n", "curl: (7) refused"},
		"quotes the engine wrote":   {`{"error":"not_found"}`, `{'error':'not_found'}`},
		"nothing at all":            {"\n  \n", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := firstLine([]byte(tc.in)); got != tc.want {
				t.Errorf("firstLine = %q, want %q", got, tc.want)
			}
		})
	}
}
