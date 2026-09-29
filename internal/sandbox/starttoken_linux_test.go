//go:build linux

package sandbox

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// statLine builds a /proc/<pid>/stat line with the given comm and start
// time, and the right number of fields between them. Field 22 is the start
// time; the nineteen before it are the ones the parser has to walk past.
func statLine(comm, startTime string) string {
	fields := make([]string, 0, 20)
	fields = append(fields, "S") // state, field 3
	for i := range 18 {          // ppid through itrealvalue, fields 4–21
		fields = append(fields, strconv.Itoa(i+1))
	}
	fields = append(fields, startTime) // starttime, field 22
	return "7 (" + comm + ") " + strings.Join(fields, " ") + " 0 0 0\n"
}

// TestTheStatParseCountsFromTheLastBracket: the comm field is a name the
// process chose, and the parser's own comment says why it counts from the
// last ')' rather than splitting the line — a process named "(evil) 1 2 3"
// would otherwise shift every index after it, and the token read out would
// be some other field of the line.
//
// That defence had no test, because it could not have one while the parse
// and the read of /proc were one function: no pid produces a hostile comm
// on demand. Splitting them is what makes it assertable, and the case below
// is the exact name the comment names.
func TestTheStatParseCountsFromTheLastBracket(t *testing.T) {
	for name, comm := range map[string]string{
		"an ordinary name":        "probavi",
		"a name with spaces":      "my drill",
		"the name in the comment": "(evil) 1 2 3",
		"a name that is brackets": "))))",
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := startTokenFrom(statLine(comm, "918273"))
			if !ok {
				t.Fatalf("startTokenFrom refused a well-formed line for comm %q", comm)
			}
			if got != "918273" {
				t.Errorf("token = %q, want 918273 — the parse landed on another field", got)
			}
		})
	}
}

// TestTheStatParseRefusesALineItCannotRead: the bound on the field count is
// load-bearing. Without it a line shorter than the parser expects indexes
// past the end of the slice and panics, inside the orphan sweep — and a
// sweep that panics leaves every sandbox it had not reached yet, which is
// the failure the sweep exists to prevent.
func TestTheStatParseRefusesALineItCannotRead(t *testing.T) {
	short := strings.Fields(statLine("probavi", "918273"))
	for name, raw := range map[string]string{
		"nothing at all":        "",
		"no closing bracket":    "7 (probavi S 1 2 3",
		"one field short":       strings.Join(short[:len(short)-4], " "),
		"comm and nothing else": "7 (probavi)",
	} {
		t.Run(name, func(t *testing.T) {
			if got, ok := startTokenFrom(raw); ok {
				t.Errorf("startTokenFrom(%q) = %q, true; want no token from a line it cannot read", raw, got)
			}
		})
	}
}

// TestALiveProcessHasAStartTokenOnLinux: every other case that reads a real
// process skips when no token is available, because the token is optional
// by design — a platform without one falls back to the pid rule. On Linux
// it is not optional: /proc is where it comes from and /proc is there.
//
// The distinction matters more than it looks. A test that skips on a false
// answer cannot tell "this platform has no token" from "the read was
// reversed and now nothing has one", and a mutation run found exactly that:
// inverting the guard on the read makes every process tokenless, every
// owner id a bare pid, and every recycled pid a live owner again — with the
// portable cases all skipping quietly.
func TestALiveProcessHasAStartTokenOnLinux(t *testing.T) {
	token, ok := processStartToken(os.Getpid())
	if !ok {
		t.Fatal("no start token for this process; on Linux the token is not optional")
	}
	if token == "" {
		t.Error("start token is empty")
	}
}
