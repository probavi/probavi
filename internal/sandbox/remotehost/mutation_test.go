package remotehost

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/probavi/probavi/internal/sandbox"
)

// mutation_test.go holds the cases a mutation run found missing: each one
// is a change to the code that every other test accepted. They are here
// together because that is what they have in common — a gap in what the
// suite asserts rather than a gap in what it covers.

// TestTheSystemdFloorIsAFloor: the spec names a minimum systemd version and
// this probe is what keeps a target below it from failing obscurely in the
// middle of a restore instead of clearly at first contact. Both ends of that
// check were unasserted.
//
// The version line was only ever fed in its chatty form. `systemctl
// --version` prints the release in brackets on most builds and omits it on
// some, and the shorter form is the one a parser gets wrong — two fields is
// all the rule needs and all a stripped build offers.
func TestTheSystemdFloorIsAFloor(t *testing.T) {
	for name, tc := range map[string]struct {
		stdout  string
		wantErr bool
	}{
		"the floor exactly":       {"systemd 244 (244.1)\n", false},
		"one below the floor":     {"systemd 243 (243)\n", true},
		"one above the floor":     {"systemd 245 (245)\n", false},
		"a line with no release":  {"systemd 255\n", false},
		"a line with no version":  {"systemd\n", true},
		"something else entirely": {"bash 5.2\n", true},
	} {
		t.Run(name, func(t *testing.T) {
			p, _ := testProvider(t, response{stdout: tc.stdout})
			err := p.probeSystemd(context.Background())
			if tc.wantErr && err == nil {
				t.Errorf("probeSystemd accepted %q", tc.stdout)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("probeSystemd(%q) = %v, want it accepted", tc.stdout, err)
			}
		})
	}
}

// TestAnExecIsBoundedWhenItAskedToBe: zero means the caller asked for no
// bound, so attaching one hands every exec a context that has already
// expired; and a bound the guard steps past is a command the drill's own
// clock cannot reach — which here means an ssh that outlives the drill
// while its remote unit keeps running.
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
			sbx := testSandbox(p)
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

// TestASuffixThatDoesNotRepeat: the suffix keeps two workspaces created in
// one second from colliding on a name, and on a bare host a collision is
// two drills in one directory. The fallback for a failing crypto/rand is
// the pid, which is the same for every sandbox this process makes.
func TestASuffixThatDoesNotRepeat(t *testing.T) {
	seen := map[string]bool{}
	for range 8 {
		s := randomSuffix()
		if s == "" || seen[s] {
			t.Fatalf("suffix %q is empty or repeated; workspaces would collide", s)
		}
		seen[s] = true
	}
}

// TestFactsAreAbsentWhenTheTargetDidNotAnswer: a non-zero exit is not an
// error by the Runner contract — err stays nil — so a check reading only
// err believes whatever the target printed on its way to failing. What is
// at stake is a record field, and a parse of an error message is worse
// there than an honest absence.
func TestFactsAreAbsentWhenTheTargetDidNotAnswer(t *testing.T) {
	for name, r := range map[string]response{
		"a non-zero exit that still printed": {stdout: "2147483648 150%", stderr: "no such unit", exit: 1},
		"a transport failure that printed":   {stdout: "2147483648 150%", err: errors.New("ssh died")},
	} {
		t.Run(name, func(t *testing.T) {
			p, _ := testProvider(t, r)
			if got := testSandbox(p).Facts(context.Background()); got != (sandbox.Facts{}) {
				t.Errorf("facts = %+v, want none: the target did not answer", got)
			}
		})
	}
}

// TestFactsSurviveATargetThatAnsweredWithLess: the script asks for two
// values and a slice with no CPU quota answers with one. Each index is
// guarded for that reason, and the guards are the difference between a
// missing field and a panic inside a drill that had already restored.
func TestFactsSurviveATargetThatAnsweredWithLess(t *testing.T) {
	p, _ := testProvider(t, response{stdout: "2147483648\n"})
	facts := testSandbox(p).Facts(context.Background())
	if facts.MemoryBytes == nil || *facts.MemoryBytes != 2147483648 {
		t.Errorf("memory = %v, want the one value the target gave", facts.MemoryBytes)
	}
	if facts.CPUsMilli != nil {
		t.Errorf("cpu = %d, want none: the slice stated no quota", *facts.CPUsMilli)
	}
}

// TestAReportedLimitIsKeptWhenItIsOne: both conversions turn what systemd
// reported into a record field, and both refuse anything that is not
// positive so that "no limit" stays an absence rather than becoming a limit
// of zero. The smallest thing that rule admits is one, and neither
// conversion was asserted there.
func TestAReportedLimitIsKeptWhenItIsOne(t *testing.T) {
	t.Run("one byte of memory", func(t *testing.T) {
		if got := bytesOrNil("1"); got == nil || *got != 1 {
			t.Errorf("bytesOrNil(\"1\") = %v, want 1", got)
		}
	})
	for in, want := range map[string]int64{
		"1%":    10, // the smallest whole percentage
		"0.1%":  1,  // the smallest quota that survives the conversion
		"150%":  1500,
		"0.05%": 0, // rounds away: absent, never a quota of zero
		"0%":    0,
		"150":   0, // no suffix: not a quota at all
	} {
		t.Run("a quota of "+in, func(t *testing.T) {
			got := quotaMilli(in)
			switch {
			case want == 0 && got != nil:
				t.Errorf("quotaMilli(%q) = %d, want nothing recorded", in, *got)
			case want != 0 && (got == nil || *got != want):
				t.Errorf("quotaMilli(%q) = %v, want %d", in, got, want)
			}
		})
	}
}

// TestACreateThatCouldNotBeUndoneSaysSo: the slice and the workspace exist
// on the target and nothing outside the call knows their name, so a failure
// to destroy leaves both behind on a host Probavi does not otherwise touch.
// The log line is the whole of what an operator gets.
func TestACreateThatCouldNotBeUndoneSaysSo(t *testing.T) {
	p, _ := testProvider(t,
		versionOK,                             // systemctl --version
		userOK,                                // id -un
		response{err: errors.New("ssh died")}, // setup fails
		response{err: errors.New("ssh died")}, // destroy fails too
	)
	log := &bytes.Buffer{}
	p.logger = slog.New(slog.NewTextHandler(log, nil))

	if _, err := p.Create(context.Background(), map[string]string{}); err == nil {
		t.Fatal("Create reported success although the sandbox was never set up")
	}
	if !strings.Contains(log.String(), "destroy after failed create") {
		t.Errorf("logged %q, want the leftover slice named", log)
	}
}
