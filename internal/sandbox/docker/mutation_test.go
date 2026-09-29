package docker

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/probavi/probavi/internal/sandbox"
)

// mutation_test.go holds the cases a mutation run found missing: each one
// is a change to the code that every other test accepted. They are here
// together because that is what they have in common — a gap in what the
// suite asserts rather than a gap in what it covers.

// TestAnExecIsBoundedWhenItAskedToBe: a request's timeout decides whether
// the subprocess gets a deadline at all, and neither end of that decision
// was asserted. Zero means the caller asked for no bound, so attaching one
// gives every exec a context that has already expired; and a bound the
// guard steps past is a command the drill's clock cannot reach, which is
// how a restore runs until the drill deadline instead of its own.
func TestAnExecIsBoundedWhenItAskedToBe(t *testing.T) {
	for name, tc := range map[string]struct {
		timeout time.Duration
		want    bool
	}{
		"no timeout asked for": {0, false},
		"the smallest timeout": {1, true},
		"an ordinary timeout":  {time.Second, true},
	} {
		t.Run(name, func(t *testing.T) {
			p, fake := testProvider(t, response{})
			sbx := &Sandbox{id: "abc123", p: p}
			if _, err := sbx.Exec(context.Background(), sandbox.ExecRequest{
				Argv: []string{"true"}, Timeout: tc.timeout,
			}); err != nil {
				t.Fatalf("Exec: %v", err)
			}
			if len(fake.bounded) != 1 {
				t.Fatalf("calls = %d, want one exec", len(fake.bounded))
			}
			if fake.bounded[0] != tc.want {
				t.Errorf("context carried a deadline = %v, want %v", fake.bounded[0], tc.want)
			}
		})
	}
}

// TestOneByteOfStdinStillOpensIt: docker exec delivers stdin only when it
// is asked to with -i, so the flag is not an optimisation. A guard that
// steps past a single byte drops the smallest input there is, and the
// command reads an empty stream while the caller believes it was fed.
func TestOneByteOfStdinStillOpensIt(t *testing.T) {
	p, fake := testProvider(t, response{})
	sbx := &Sandbox{id: "abc123", p: p}
	if _, err := sbx.Exec(context.Background(), sandbox.ExecRequest{
		Argv: []string{"cat"}, Stdin: []byte("x"),
	}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !slices.Contains(fake.calls[0], "-i") {
		t.Errorf("argv = %v, want -i for a request carrying stdin", fake.calls[0])
	}
	if fake.stdins[0] != "x" {
		t.Errorf("stdin = %q, want the byte it was given", fake.stdins[0])
	}
}

// TestASuffixThatDoesNotRepeat: the suffix keeps two sandboxes created in
// one second from colliding on a name. The fallback for a failing
// crypto/rand is the pid, which is the same for every container this
// process makes — correct as a fallback, and a collision generator if it
// becomes the normal path.
func TestASuffixThatDoesNotRepeat(t *testing.T) {
	seen := map[string]bool{}
	for range 8 {
		s := randomSuffix()
		if s == "" {
			t.Fatal("empty suffix")
		}
		if seen[s] {
			t.Fatalf("suffix %q repeated; names would collide", s)
		}
		seen[s] = true
	}
}

// TestFactsAreAbsentWhenInspectDidNotAnswer: a non-zero exit is not an
// error by the Runner contract — err stays nil and the code is returned —
// so a check that only looks at err believes whatever inspect printed on
// its way to failing. What is at stake is a record field: sandbox.facts
// says what the sandbox actually was, and a parse of an error message is
// worse than an honest absence.
func TestFactsAreAbsentWhenInspectDidNotAnswer(t *testing.T) {
	for name, r := range map[string]response{
		"a non-zero exit with no error": {stdout: "alpine|123|456", stderr: "no such object", exit: 1},
		"a spawn failure":               {err: errors.New("docker not found")},
	} {
		t.Run(name, func(t *testing.T) {
			p, _ := testProvider(t, r)
			sbx := &Sandbox{id: "abc123", p: p}
			if got := (sbx.Facts(context.Background())); got != (sandbox.Facts{}) {
				t.Errorf("facts = %+v, want none: inspect did not answer", got)
			}
		})
	}
}

// TestPositiveOrNilTakesEveryPositiveAndNothingElse: this is where docker's
// reported limits become sandbox.resources in a signed record. Zero means
// "no limit applied" and has to stay absent rather than become a limit of
// zero, which the comment says — and the other end matters as much: a value
// too large to parse comes back from ParseInt clamped to the largest int
// alongside its error, so a guard that reads the error and the sign the
// wrong way round records a limit nobody set.
func TestPositiveOrNilTakesEveryPositiveAndNothingElse(t *testing.T) {
	for name, tc := range map[string]struct {
		in   string
		unit int64
		want *int64
	}{
		"the smallest positive":     {"1", 1, ptr(1)},
		"an ordinary byte count":    {"2147483648", 1, ptr(2147483648)},
		"nanocpus to milli":         {"1500000000", 1_000_000, ptr(1500)},
		"docker's no limit applied": {"0", 1, nil},
		"a negative":                {"-1", 1, nil},
		"not a number":              {"unlimited", 1, nil},
		"empty":                     {"", 1, nil},
		"too large for an int":      {"99999999999999999999", 1, nil},
		"smaller than one unit":     {"999999", 1_000_000, nil},
	} {
		t.Run(name, func(t *testing.T) {
			got := positiveOrNil(tc.in, tc.unit)
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("positiveOrNil(%q, %d) = %d, want nothing recorded", tc.in, tc.unit, *got)
			case tc.want != nil && got == nil:
				t.Errorf("positiveOrNil(%q, %d) = nothing, want %d", tc.in, tc.unit, *tc.want)
			case tc.want != nil && *got != *tc.want:
				t.Errorf("positiveOrNil(%q, %d) = %d, want %d", tc.in, tc.unit, *got, *tc.want)
			}
		})
	}
}

func ptr(n int64) *int64 { return &n }

// TestACreateThatCouldNotBeUndoneSaysSo: the container exists and nothing
// outside the call knows its id, so this is the one path where a failure to
// destroy leaks a sandbox that no later sweep can attribute. The log line
// is the whole of what an operator gets, and the condition deciding whether
// it is written accepted being reversed.
func TestACreateThatCouldNotBeUndoneSaysSo(t *testing.T) {
	p, _ := testProvider(t,
		response{stdout: "abc123\n"},                   // docker run
		response{err: errors.New("daemon gone")},       // inspect: await fails
		response{err: errors.New("daemon still gone")}, // docker rm: destroy fails
	)
	log := &bytes.Buffer{}
	p.logger = slog.New(slog.NewTextHandler(log, nil))

	if _, err := p.Create(context.Background(), map[string]string{"image": "alpine:3"}); err == nil {
		t.Fatal("Create reported success although the container never ran")
	}
	if !strings.Contains(log.String(), "destroy after failed create") {
		t.Errorf("logged %q, want the leaked container named", log)
	}
	if !strings.Contains(log.String(), "abc123") {
		t.Errorf("logged %q, want it to carry the container id nothing else knows", log)
	}
}
