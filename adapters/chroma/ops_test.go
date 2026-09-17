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

// TestStartEngineCarriesTheEnginesOwnWords: the sandbox is destroyed when
// the drill ends, so a start that failed has to bring the engine's own
// line with it or the record carries nothing anybody can act on.
func TestStartEngineCarriesTheEnginesOwnWords(t *testing.T) {
	paths := sandboxPaths{data: "/scratch/probavi-chroma/data", log: "/scratch/probavi-chroma/engine.log"}
	t.Run("a server that came up", func(t *testing.T) {
		seconds, perr := startEngine(context.Background(),
			answeredCore(t, said(0, "", ""), said(0, "", "")), paths)
		if perr != nil || seconds < 0 {
			t.Errorf("startEngine = %v, %+v, want the wait it measured", seconds, perr)
		}
	})
	t.Run("a server that would not start", func(t *testing.T) {
		_, perr := startEngine(context.Background(),
			answeredCore(t, said(1, "", "bash: chroma: command not found\n")), paths)
		if perr == nil || perr.Code != "engine_not_ready" || !strings.Contains(perr.Message, "command not found") {
			t.Errorf("perr = %+v, want engine_not_ready carrying the sandbox's words", perr)
		}
	})
	t.Run("a core that went away mid-call", func(t *testing.T) {
		if _, perr := startEngine(context.Background(), answeredCore(t), paths); perr == nil ||
			perr.Code != "internal" {
			t.Errorf("perr = %+v, want the harness's own failure", perr)
		}
	})
}

// TestStartupDiagnosisQuotesTheEngineOrSaysNothing: a diagnosis that could
// not be read must not be invented, and one that could is quoted as the
// engine wrote it.
func TestStartupDiagnosisQuotesTheEngineOrSaysNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		answers []any
		want    string
	}{
		"the log had something to say": {
			[]any{said(0, "\nERROR: unable to open database file\nnext line\n", "")},
			" (engine said: ERROR: unable to open database file)",
		},
		"the log was empty":              {[]any{said(0, "  \n\n", "")}, ""},
		"a core that went away mid-call": {nil, ""},
	} {
		t.Run(name, func(t *testing.T) {
			got := startupDiagnosis(context.Background(), answeredCore(t, tc.answers...), "/scratch/engine.log")
			if got != tc.want {
				t.Errorf("startupDiagnosis = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTheVerdictIsTheQueryRatherThanTheCount pins what the verdict script
// answers with. Deleting a collection's whole vector index leaves the
// record count answering exactly right while the nearest-neighbour search
// returns nothing (measured), so the exit code carries the verdict and the
// count on success is only a number to report.
func TestTheVerdictIsTheQueryRatherThanTheCount(t *testing.T) {
	for name, tc := range map[string]struct {
		answers []any
		records int
		code    string
		message string
	}{
		"collections that answer their own vectors": {
			[]any{said(0, "2200\n", "")}, 2200, "", "",
		},
		"a count the script did not normalise": {
			[]any{said(0, "not a number\n", "")}, 0, "", "",
		},
		"a directory serving no collections": {
			[]any{said(exitNoCollections, "", "")}, 0, "restore_failed", "serves no collections",
		},
		"an index that did not survive the backup": {
			[]any{said(exitIndexLost, "", "collection docs returned 0 of 3 neighbours\n")},
			0, "source_corrupt", "vector index did not survive",
		},
		"a database that could not be read": {
			[]any{said(1, "", "sqlite3.DatabaseError: file is not a database\n")},
			0, "restore_failed", "file is not a database",
		},
		"a core that went away mid-call": {nil, 0, "internal", "closed the stream"},
	} {
		t.Run(name, func(t *testing.T) {
			records, perr := verifyRestore(context.Background(), answeredCore(t, tc.answers...))
			if tc.code == "" {
				if perr != nil || records != tc.records {
					t.Errorf("verifyRestore = %d, %+v, want %d", records, perr, tc.records)
				}
				return
			}
			if perr == nil || perr.Code != tc.code || !strings.Contains(perr.Message, tc.message) {
				t.Errorf("perr = %+v, want %s mentioning %q", perr, tc.code, tc.message)
			}
		})
	}
}

// TestFirstLineStaysReadable: captured output lands in evidence error
// fields, so it crosses as one line, bounded, and never empty — an error
// that says nothing at all still has to say that.
func TestFirstLineStaysReadable(t *testing.T) {
	long := strings.Repeat("x", 400)
	for name, tc := range map[string]struct{ in, want string }{
		"the first line of several":    {"  ERROR: no such table\nand more\n", "ERROR: no such table"},
		"no output at all":             {"  \n\n", "no output"},
		"a line longer than the field": {long, long[:300] + "..."},
	} {
		t.Run(name, func(t *testing.T) {
			if got := firstLine([]byte(tc.in)); got != tc.want {
				t.Errorf("firstLine = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAtoiReadsOnlyANonNegativeNumber: the verdict script normalises its
// fields, so anything else is a malformed answer that must fail the
// comparison rather than the parse.
func TestAtoiReadsOnlyANonNegativeNumber(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"2200\n", 2200}, {" 7 ", 7}, {"0", 0}, {"", 0}, {"many", 0}, {"-3", 0}, {"1.5", 0},
	} {
		if got := atoi(tc.in); got != tc.want {
			t.Errorf("atoi(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
