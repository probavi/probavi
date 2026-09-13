// Command coverage reports statement coverage per area of the repository
// and fails when an area sits below the floor committed beside it.
//
// It exists because the ratchet it replaces measured one area and was read
// as measuring the repository. `-coverpkg=./internal/...` covered 11.7k of
// the 56k lines that ship, leaving adapters/ — 79% of the production code —
// with no floor at all, and the newest adapter 36 points below the gated
// core with nothing to say so. AGENTS.md §3.1 states the rule without a
// qualifier: coverage is measured and enforced in CI, and must not
// decrease. This measures what that sentence claims.
//
// Every package in the profile must fall under a declared area. That is the
// point rather than a strictness: a new top-level directory then arrives
// with a decision about its floor attached, instead of arriving ungated and
// staying that way because nobody was asked.
//
// It is a repository tool, not a shipped binary.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// floor is one area of the repository and the statement coverage it may
// not fall below.
type floor struct {
	// Area is a path prefix relative to the module root, matched by whole
	// path segments. The longest matching area wins, so "internal" and a
	// stricter "internal/evidence" can both be declared.
	Area string
	// Min is the percentage the area must reach.
	Min float64
}

// block is one coverage block's identity, used to fold the repeats a
// profile carries when several test binaries instrument the same package.
type block struct {
	file string
	span string
}

// tally accumulates one area's statements.
type tally struct {
	total, covered int
}

// profileLine matches a coverage profile entry: the instrumented file, the
// block's span, the number of statements in it, and how often it ran.
var profileLine = regexp.MustCompile(`^(.+):(\d+\.\d+,\d+\.\d+) (\d+) (\d+)$`)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "coverage: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("coverage", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", ".", "repository root")
	profilePath := fs.String("profile", "cover.out", "coverage profile to read")
	floorsPath := fs.String("floors", ".coverage-floor", "committed minimums, one area per line")
	if err := fs.Parse(args); err != nil {
		return err
	}

	floors, err := readFloors(under(*root, *floorsPath))
	if err != nil {
		return err
	}
	module, err := modulePath(under(*root, "go.mod"))
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(under(*root, *profilePath))
	if err != nil {
		return fmt.Errorf("read coverage profile: %w", err)
	}
	tallies, err := measure(string(raw), module, floors)
	if err != nil {
		return err
	}
	return report(stdout, floors, tallies)
}

// under resolves a path against the repository root, leaving an absolute
// one alone. filepath.Join would quietly turn "/tmp/cover.out" into
// "tmp/cover.out" against the default root of ".", and the error that
// follows names a path nobody passed.
func under(root, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Clean(filepath.Join(root, p))
}

// measure folds a coverage profile into one tally per declared area.
func measure(profile, module string, floors []floor) (map[string]*tally, error) {
	counted, err := blocks(profile)
	if err != nil {
		return nil, err
	}
	tallies := make(map[string]*tally, len(floors))
	for _, f := range floors {
		tallies[f.Area] = &tally{}
	}
	unclaimed := map[string]bool{}
	for b, e := range counted {
		rel, ok := strings.CutPrefix(b.file, module+"/")
		if !ok {
			// A profile can only name packages of the module it was
			// produced for; anything else means the wrong file was read.
			return nil, fmt.Errorf("coverage profile names %s, which is outside module %s", b.file, module)
		}
		area, ok := areaOf(rel, floors)
		if !ok {
			unclaimed[path.Dir(rel)] = true
			continue
		}
		t := tallies[area]
		t.total += e.stmts
		if e.ran {
			t.covered += e.stmts
		}
	}
	if len(unclaimed) > 0 {
		dirs := sortedKeys(unclaimed)
		return nil, fmt.Errorf("no floor covers %s — every package needs one, so add the area to the floors file "+
			"with the coverage it has today rather than leaving it unmeasured", strings.Join(dirs, ", "))
	}
	return tallies, nil
}

// entry is one folded coverage block: how many statements it holds, and
// whether any test binary ran it.
type entry struct {
	stmts int
	ran   bool
}

// blocks parses a profile, folding the repeated entries it carries for the
// same block. A profile produced with -coverpkg holds one entry per test
// binary that instrumented the package, and a block that ran under any of
// them ran.
func blocks(profile string) (map[block]entry, error) {
	out := map[block]entry{}
	sc := bufio.NewScanner(strings.NewReader(profile))
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "mode:") {
			continue
		}
		m := profileLine.FindStringSubmatch(text)
		if m == nil {
			return nil, fmt.Errorf("coverage profile line %d is not a profile entry: %q", line, text)
		}
		stmts, serr := strconv.Atoi(m[3])
		count, cerr := strconv.Atoi(m[4])
		if serr != nil || cerr != nil {
			return nil, fmt.Errorf("coverage profile line %d has unreadable counts: %q", line, text)
		}
		key := block{file: m[1], span: m[2]}
		out[key] = entry{stmts: stmts, ran: out[key].ran || count > 0}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read coverage profile: %w", err)
	}
	return out, nil
}

// areaOf returns the declared area a module-relative file belongs to. The
// longest matching prefix wins, so a stricter area declared inside a
// broader one takes precedence.
func areaOf(rel string, floors []floor) (string, bool) {
	best, found := "", false
	for _, f := range floors {
		if rel == f.Area || strings.HasPrefix(rel, f.Area+"/") {
			if len(f.Area) > len(best) {
				best, found = f.Area, true
			}
		}
	}
	return best, found
}

// report prints each area's coverage and returns an error naming every one
// that fell below its floor.
func report(stdout io.Writer, floors []floor, tallies map[string]*tally) error {
	var below []string
	for _, f := range floors {
		t := tallies[f.Area]
		pct := percent(t)
		fmt.Fprintf(stdout, "%-28s %6.1f%%  (floor %.1f%%, %d/%d statements)\n",
			f.Area, pct, f.Min, t.covered, t.total)
		if t.total == 0 {
			below = append(below, fmt.Sprintf("%s has no statements in the profile — the area is misspelled "+
				"or its packages were not built with coverage", f.Area))
			continue
		}
		// Rounded to one decimal, so what fails is what the line above
		// printed: a floor cannot be missed by a digit nobody can see.
		if round1(pct) < f.Min {
			below = append(below, fmt.Sprintf("%s is at %.1f%%, below the committed floor of %.1f%%",
				f.Area, round1(pct), f.Min))
		}
	}
	if len(below) > 0 {
		return fmt.Errorf("coverage fell below a committed floor — add tests; never lower a floor (AGENTS.md §3.1):\n  %s",
			strings.Join(below, "\n  "))
	}
	return nil
}

func percent(t *tally) float64 {
	if t.total == 0 {
		return 0
	}
	return float64(t.covered) * 100 / float64(t.total)
}

// round1 rounds half away from zero to one decimal, matching the printed
// value.
func round1(pct float64) float64 {
	scaled := pct*10 + 0.5
	return float64(int64(scaled)) / 10
}

// readFloors parses the committed minimums: one "<area> <percent>" per
// line, blank lines and # comments ignored.
func readFloors(path string) ([]floor, error) {
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("read coverage floors: %w", err)
	}
	lines := strings.Split(string(raw), "\n")
	floors := make([]floor, 0, len(lines))
	seen := map[string]bool{}
	for n, line := range lines {
		text := strings.TrimSpace(line)
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		fields := strings.Fields(text)
		if len(fields) != 2 {
			return nil, fmt.Errorf("%s line %d: want \"<area> <percent>\", got %q", path, n+1, text)
		}
		min, perr := strconv.ParseFloat(fields[1], 64)
		if perr != nil || min < 0 || min > 100 {
			return nil, fmt.Errorf("%s line %d: %q is not a percentage", path, n+1, fields[1])
		}
		area := strings.Trim(fields[0], "/")
		if seen[area] {
			return nil, fmt.Errorf("%s line %d: area %q is declared twice", path, n+1, area)
		}
		seen[area] = true
		floors = append(floors, floor{Area: area, Min: min})
	}
	if len(floors) == 0 {
		return nil, errors.New("coverage floors file declares no areas — this gate would pass vacuously")
	}
	sort.Slice(floors, func(i, j int) bool { return floors[i].Area < floors[j].Area })
	return floors, nil
}

// modulePath reads the module path a go.mod declares, which is the prefix
// every file in the coverage profile carries.
func modulePath(path string) (string, error) {
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("read go.mod: %w", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			if p := strings.TrimSpace(rest); p != "" {
				return p, nil
			}
		}
	}
	return "", fmt.Errorf("%s declares no module path", path)
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
