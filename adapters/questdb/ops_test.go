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
// with no answers is one whose stream is gone: the step that calls it gets
// the harness's own failure, which is how a step behaves when the core
// went away mid-operation.
func answeredCore(t *testing.T, answers ...execValue) *core {
	t.Helper()
	var lines strings.Builder
	for i, val := range answers {
		raw, err := json.Marshal(val)
		if err != nil {
			t.Fatal(err)
		}
		lines.WriteString(sandboxResultLine(`{"call_id":"c` + strconv.Itoa(i+1) + `","ok":true,"value":` + string(raw) + `}`))
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

// TestAssertIdleOnlyRefusesOnAPositiveAnswer: the sandbox is refused when
// the probe reached a serving engine, and a probe that could not run —
// an image without curl, say — stops no drill.
func TestAssertIdleOnlyRefusesOnAPositiveAnswer(t *testing.T) {
	for name, tc := range map[string]struct {
		answers []execValue
		refused bool
		code    string
	}{
		"an engine that answered":        {[]execValue{said(0, "0\n", "")}, true, "invalid_request"},
		"an engine that did not answer":  {[]execValue{said(0, "7\n", "")}, false, ""},
		"a probe that could not be run":  {[]execValue{said(127, "", "curl: not found")}, false, ""},
		"a core that went away mid-call": {nil, true, "internal"},
	} {
		t.Run(name, func(t *testing.T) {
			perr := assertIdle(context.Background(), answeredCore(t, tc.answers...))
			if !tc.refused {
				if perr != nil {
					t.Errorf("perr = %+v, want the drill to go on", perr)
				}
				return
			}
			if perr == nil || perr.Code != tc.code {
				t.Fatalf("perr = %+v, want %s", perr, tc.code)
			}
			if tc.code == "invalid_request" && !strings.Contains(perr.Message, "sleep infinity") {
				t.Errorf("message = %q, want the parameter that fixes it", perr.Message)
			}
		})
	}
}

// TestPlaceDataRootJudgesWhatMoved: the script answers with the data root
// it made, so silence is a failure however the shell exited.
func TestPlaceDataRootJudgesWhatMoved(t *testing.T) {
	for name, tc := range map[string]struct {
		answers []execValue
		code    string
		message string
	}{
		"the data root was replaced": {[]execValue{said(0, dataRoot+"\n", "")}, "", ""},
		"the script failed": {
			[]execValue{said(1, "", "mv: cannot move: Permission denied\n")},
			"source_corrupt", "mv: cannot move",
		},
		"the script said nothing": {
			[]execValue{said(0, "  \n", "")}, "source_corrupt", "could not be made the server's data root",
		},
		"a core that went away mid-call": {nil, "internal", "closed the stream"},
	} {
		t.Run(name, func(t *testing.T) {
			seconds, perr := placeDataRoot(context.Background(), answeredCore(t, tc.answers...))
			if tc.code == "" {
				if perr != nil || seconds != 0.25 {
					t.Errorf("placeDataRoot = %v, %+v, want the measured duration", seconds, perr)
				}
				return
			}
			if perr == nil || perr.Code != tc.code || !strings.Contains(perr.Message, tc.message) {
				t.Errorf("perr = %+v, want %s mentioning %q", perr, tc.code, tc.message)
			}
		})
	}
}

// TestStartEngineSaysWhatTheEngineSaid: a server that will not start is
// not the drill's fault to guess at, so the refusal carries the engine's
// own first line.
func TestStartEngineSaysWhatTheEngineSaid(t *testing.T) {
	t.Run("a server that came up", func(t *testing.T) {
		seconds, perr := startEngine(context.Background(), answeredCore(t, said(0, "", ""), said(0, "1\n", "")))
		if perr != nil || seconds < 0 {
			t.Errorf("startEngine = %v, %+v, want the wait it measured", seconds, perr)
		}
	})
	t.Run("a server that would not start", func(t *testing.T) {
		_, perr := startEngine(context.Background(),
			answeredCore(t, said(1, "", "\nio.questdb.cairo.CairoException: could not open read-write\n")))
		if perr == nil || perr.Code != "engine_not_ready" || !strings.Contains(perr.Message, "CairoException") {
			t.Errorf("perr = %+v, want engine_not_ready carrying the engine's words", perr)
		}
	})
	t.Run("a core that went away mid-call", func(t *testing.T) {
		if _, perr := startEngine(context.Background(), answeredCore(t)); perr == nil || perr.Code != "internal" {
			t.Errorf("perr = %+v, want the harness's own failure", perr)
		}
	})
}

// TestStartupDiagnosisQuotesTheEngineOrSaysNothing: the sandbox is
// destroyed when the drill ends, so a refusal that does not carry the
// engine's own words carries nothing anybody can act on — and a
// diagnosis that could not be read must not invent one.
func TestStartupDiagnosisQuotesTheEngineOrSaysNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		answers []execValue
		want    string
	}{
		"the log had something to say": {
			[]execValue{said(0, "\n2026-09-17T10:00:00 E i.q.c.CairoEngine could not mmap\nnext line\n", "")},
			" — it said: 2026-09-17T10:00:00 E i.q.c.CairoEngine could not mmap",
		},
		"there was no log to read":       {[]execValue{said(1, "", "no such file")}, ""},
		"the log was empty":              {[]execValue{said(0, "  \n\n", "")}, ""},
		"a core that went away mid-call": {nil, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := startupDiagnosis(context.Background(), answeredCore(t, tc.answers...)); got != tc.want {
				t.Errorf("startupDiagnosis = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAssertRestoredRefusesAWellFormedZero: an engine that came up on a
// data root holding tables and serves none of them has restored nothing,
// and reporting that green is the failure this project exists to prevent.
func TestAssertRestoredRefusesAWellFormedZero(t *testing.T) {
	for name, tc := range map[string]struct {
		tables  int
		answers []execValue
		code    string
		message string
	}{
		"a backup that holds no user table": {0, nil, "", ""},
		"tables served":                     {2, []execValue{said(0, "2\n", "")}, "", ""},
		"a server serving none of them": {
			2, []execValue{said(0, "0\n", "")}, "source_corrupt", "holds 2 tables and the restored server serves none",
		},
		"one table, and the message says so": {
			1, []execValue{said(0, "0\n", "")}, "source_corrupt", "holds 1 table and",
		},
		"a server that would not say": {
			1, []execValue{said(1, "", "curl: (7) Failed to connect\n")}, "restore_failed", "curl: (7)",
		},
		"an answer that is not a count": {
			1, []execValue{said(0, "many\n", "")}, "restore_failed", "did not answer a table count",
		},
		"a core that went away mid-call": {1, nil, "internal", "closed the stream"},
	} {
		t.Run(name, func(t *testing.T) {
			perr := assertRestored(context.Background(), answeredCore(t, tc.answers...),
				&resolvedSource{userTables: tc.tables})
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

// TestMalformedPayloadsAreInvalidRequests: a payload the adapter cannot
// read is the core's mistake, and it is answered rather than crashed on.
func TestMalformedPayloadsAreInvalidRequests(t *testing.T) {
	for _, op := range []string{"provision", "healthcheck"} {
		t.Run(op, func(t *testing.T) {
			line, calls, exit := driveOp(t, op, `"not an object"`, func(verbCall) (any, *protoError) {
				t.Error("the sandbox was touched for a payload that could not be read")
				return okExec(0), nil
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
}

// TestHealthcheckSaysWhyItIsUnhealthy: a healthcheck that answers false
// without a reason leaves an operator nothing to act on, so the engine's
// own words are used where there are any and the exit code where there
// are none.
func TestHealthcheckSaysWhyItIsUnhealthy(t *testing.T) {
	for name, tc := range map[string]struct {
		answer any
		want   string
	}{
		"the engine explained itself": {said(7, "", "curl: (7) Failed to connect to localhost\n"), "curl: (7)"},
		"the engine said nothing":     {said(28, "", ""), "curl exit 28"},
	} {
		t.Run(name, func(t *testing.T) {
			line, _, exit := driveOp(t, "healthcheck", `{"state":{"data_root":"`+dataRoot+`"}}`,
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
			if got.Healthy || !strings.Contains(got.Detail, tc.want) {
				t.Errorf("healthy = %v, detail = %q, want an unhealthy answer mentioning %q",
					got.Healthy, got.Detail, tc.want)
			}
		})
	}
}
