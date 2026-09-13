package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const testModule = "github.com/probavi/probavi"

// threeAreas is the shape of the committed floors file.
var threeAreas = []floor{{Area: "adapters", Min: 80}, {Area: "cmd", Min: 80}, {Area: "internal", Min: 80}}

// profileOf renders a coverage profile from "path stmts count" triples, so a
// test states the statements it means rather than the text around them.
func profileOf(entries ...string) string {
	var b strings.Builder
	b.WriteString("mode: atomic\n")
	for i, e := range entries {
		f := strings.Fields(e)
		line := strconv.Itoa(i + 1)
		b.WriteString(f[0] + ":" + line + ".1," + line + ".9 " + f[1] + " " + f[2] + "\n")
	}
	return b.String()
}

func TestMeasureCountsStatementsPerArea(t *testing.T) {
	p := profileOf(
		testModule+"/internal/core/core.go 10 1",
		testModule+"/internal/core/other.go 5 0",
		testModule+"/adapters/postgres/ops.go 20 1",
		testModule+"/cmd/probavi/run.go 4 0",
	)
	got, err := measure(p, testModule, threeAreas)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	for _, tc := range []struct {
		area           string
		covered, total int
	}{
		{"internal", 10, 15},
		{"adapters", 20, 20},
		{"cmd", 0, 4},
	} {
		if got[tc.area].covered != tc.covered || got[tc.area].total != tc.total {
			t.Errorf("%s = %d/%d, want %d/%d", tc.area,
				got[tc.area].covered, got[tc.area].total, tc.covered, tc.total)
		}
	}
}

// TestMeasureFoldsRepeatedBlocks pins the reason this tool parses the
// profile itself. With -coverpkg every test binary instruments every
// package, so one block appears once per binary — uncovered by most of them.
// Counting the entries instead of folding them would report a fraction of
// the real coverage and fail a floor that nothing had fallen below.
func TestMeasureFoldsRepeatedBlocks(t *testing.T) {
	p := "mode: atomic\n" +
		testModule + "/internal/core/core.go:10.1,12.9 3 0\n" +
		testModule + "/internal/core/core.go:10.1,12.9 3 7\n" +
		testModule + "/internal/core/core.go:10.1,12.9 3 0\n"
	got, err := measure(p, testModule, threeAreas)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if got["internal"].total != 3 || got["internal"].covered != 3 {
		t.Errorf("folded block = %d/%d statements, want 3/3 — the block ran under one binary, so it ran",
			got["internal"].covered, got["internal"].total)
	}
}

func TestMeasureUsesTheLongestMatchingArea(t *testing.T) {
	floors := []floor{{Area: "internal", Min: 80}, {Area: "internal/evidence", Min: 99}}
	p := profileOf(
		testModule+"/internal/core/core.go 10 1",
		testModule+"/internal/evidence/store.go 6 0",
	)
	got, err := measure(p, testModule, floors)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if got["internal"].total != 10 {
		t.Errorf("internal = %d statements, want 10 — evidence belongs to its own area", got["internal"].total)
	}
	if got["internal/evidence"].total != 6 {
		t.Errorf("internal/evidence = %d statements, want 6", got["internal/evidence"].total)
	}
}

// TestMeasureRefusesAPackageNoFloorCovers is the property that keeps a new
// top-level directory from arriving ungated.
func TestMeasureRefusesAPackageNoFloorCovers(t *testing.T) {
	p := profileOf(testModule + "/spike/main.go 9 0")
	_, err := measure(p, testModule, threeAreas)
	if err == nil {
		t.Fatal("measure accepted a package no floor covers")
	}
	if !strings.Contains(err.Error(), "spike") {
		t.Errorf("error does not name the uncovered package: %v", err)
	}
}

func TestMeasureRefusesAProfileFromAnotherModule(t *testing.T) {
	p := profileOf("example.com/other/main.go 3 1")
	if _, err := measure(p, testModule, threeAreas); err == nil {
		t.Fatal("measure accepted a profile from another module")
	}
}

func TestBlocksRefusesWhatIsNotAProfile(t *testing.T) {
	for _, tc := range []struct{ name, profile string }{
		{"prose", "mode: atomic\nthis is not a profile entry\n"},
		{"missing counts", "mode: atomic\n" + testModule + "/a/b.go:1.1,2.9\n"},
		{"counts out of range", "mode: atomic\n" + testModule + "/a/b.go:1.1,2.9 99999999999999999999 1\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := blocks(tc.profile); err == nil {
				t.Error("blocks accepted it")
			}
		})
	}
}

func TestReportNamesEveryAreaBelowItsFloor(t *testing.T) {
	floors := []floor{{Area: "adapters", Min: 90}, {Area: "internal", Min: 90}}
	tallies := map[string]*tally{
		"adapters": {total: 100, covered: 82},
		"internal": {total: 100, covered: 96},
	}
	var out bytes.Buffer
	err := report(&out, floors, tallies)
	if err == nil {
		t.Fatal("report passed an area below its floor")
	}
	if !strings.Contains(err.Error(), "adapters") {
		t.Errorf("error does not name the failing area: %v", err)
	}
	if strings.Contains(err.Error(), "internal is at") {
		t.Errorf("error blames an area that is above its floor: %v", err)
	}
	if !strings.Contains(out.String(), "82.0%") {
		t.Errorf("the printed report does not show the measurement:\n%s", out.String())
	}
}

func TestReportPassesAtTheFloor(t *testing.T) {
	floors := []floor{{Area: "internal", Min: 96}}
	// 96.04% prints as 96.0 and must therefore pass a floor of 96.0: a gate
	// may not fail on a digit its own output does not show.
	tallies := map[string]*tally{"internal": {total: 10000, covered: 9604}}
	var out bytes.Buffer
	if err := report(&out, floors, tallies); err != nil {
		t.Errorf("report failed at the floor: %v", err)
	}
}

// TestReportRefusesAnEmptyArea catches a misspelled area, which would
// otherwise read as a passing 0-of-0.
func TestReportRefusesAnEmptyArea(t *testing.T) {
	err := report(&bytes.Buffer{}, []floor{{Area: "adaptors", Min: 80}},
		map[string]*tally{"adaptors": {}})
	if err == nil {
		t.Fatal("report accepted an area with no statements")
	}
	if !strings.Contains(err.Error(), "misspelled") {
		t.Errorf("error does not suggest the likely cause: %v", err)
	}
}

func TestReadFloorsAccepts(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		want          []floor
	}{
		{"areas, comments and blank lines", "# a comment\n\nadapters 82.5\ninternal  96.0\n",
			[]floor{{Area: "adapters", Min: 82.5}, {Area: "internal", Min: 96}}},
		{"sorted regardless of file order", "internal 96.0\nadapters 82.5\n",
			[]floor{{Area: "adapters", Min: 82.5}, {Area: "internal", Min: 96}}},
		{"slashes trimmed", "/cmd/ 87.0\n", []floor{{Area: "cmd", Min: 87}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readFloors(floorsFile(t, tc.content))
			if err != nil {
				t.Fatalf("readFloors: %v", err)
			}
			if !sameFloors(got, tc.want) {
				t.Errorf("readFloors = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReadFloorsRefuses(t *testing.T) {
	for _, tc := range []struct{ name, content, wantErr string }{
		{"a line that is not a pair", "adapters\n", "want"},
		{"not a percentage", "adapters high\n", "percentage"},
		{"out of range", "adapters 101\n", "percentage"},
		{"declared twice", "adapters 82.5\nadapters 90.0\n", "twice"},
		{"no areas at all", "# nothing here\n", "vacuously"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := readFloors(floorsFile(t, tc.content))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("readFloors err = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

// floorsFile writes a floors file and returns its path.
func floorsFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "floors")
	write(t, path, content)
	return path
}

func sameFloors(got, want []floor) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestReadFloorsReportsAnUnreadableFile(t *testing.T) {
	if _, err := readFloors(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("readFloors accepted a missing file")
	}
}

func TestModulePath(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "go.mod")
	write(t, good, "module github.com/probavi/probavi\n\ngo 1.25.0\n")
	got, err := modulePath(good)
	if err != nil || got != testModule {
		t.Fatalf("modulePath = %q, %v; want %q", got, err, testModule)
	}
	bad := filepath.Join(dir, "nomodule.mod")
	write(t, bad, "go 1.25.0\n")
	if _, err := modulePath(bad); err == nil {
		t.Error("modulePath accepted a file with no module line")
	}
	if _, err := modulePath(filepath.Join(dir, "absent")); err == nil {
		t.Error("modulePath accepted a missing file")
	}
}

// TestUnderLeavesAnAbsolutePathAlone pins a bug this tool shipped with for
// one commit: filepath.Join turns "/tmp/cover.out" into "tmp/cover.out"
// against the default root of ".", and the error that follows names a path
// nobody passed.
func TestUnderLeavesAnAbsolutePathAlone(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "cover.out")
	if got := under(".", abs); got != abs {
		t.Errorf("under(\".\", %q) = %q, want it unchanged", abs, got)
	}
	if got := under("/repo", "cover.out"); got != filepath.Join("/repo", "cover.out") {
		t.Errorf("under(\"/repo\", \"cover.out\") = %q", got)
	}
}

func TestRunMeasuresARepository(t *testing.T) {
	dir := newRepo(t, "adapters 50.0\ninternal 50.0\n", profileOf(
		testModule+"/internal/core/core.go 10 1",
		testModule+"/adapters/postgres/ops.go 10 1",
	))
	var out, errOut bytes.Buffer
	if err := run([]string{"-root", dir}, &out, &errOut); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, want := range []string{"adapters", "internal", "100.0%"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report does not mention %q:\n%s", want, out.String())
		}
	}
}

func TestRunFailsBelowAFloor(t *testing.T) {
	dir := newRepo(t, "internal 96.0\n", profileOf(
		testModule+"/internal/core/core.go 1 1",
		testModule+"/internal/core/other.go 1 0",
	))
	err := run([]string{"-root", dir}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("run passed at 50% against a floor of 96%")
	}
	if !strings.Contains(err.Error(), "never lower a floor") {
		t.Errorf("error does not say what to do instead: %v", err)
	}
}

func TestRunReportsWhatItCannotRead(t *testing.T) {
	for _, tc := range []struct{ name, floors, profile string }{
		{"no floors file", "", "mode: atomic\n"},
		{"no profile", "internal 96.0\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, filepath.Join(dir, "go.mod"), "module "+testModule+"\n")
			if tc.floors != "" {
				write(t, filepath.Join(dir, ".coverage-floor"), tc.floors)
			}
			if tc.profile != "" {
				write(t, filepath.Join(dir, "cover.out"), tc.profile)
			}
			if err := run([]string{"-root", dir}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
				t.Error("run accepted it")
			}
		})
	}
}

func TestRunReportsAMissingGoMod(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, ".coverage-floor"), "internal 96.0\n")
	write(t, filepath.Join(dir, "cover.out"), "mode: atomic\n")
	if err := run([]string{"-root", dir}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Error("run accepted a directory with no go.mod")
	}
}

func TestRunRejectsAnUnknownFlag(t *testing.T) {
	if err := run([]string{"-nope"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Error("run accepted an unknown flag")
	}
}

// newRepo lays out the three files run reads.
func newRepo(t *testing.T, floors, profile string) string {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "go.mod"), "module "+testModule+"\n\ngo 1.25.0\n")
	write(t, filepath.Join(dir, ".coverage-floor"), floors)
	write(t, filepath.Join(dir, "cover.out"), profile)
	return dir
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestRunReportsAPackageNoFloorCovers(t *testing.T) {
	dir := newRepo(t, "internal 50.0\n", profileOf(testModule+"/spike/main.go 4 1"))
	err := run([]string{"-root", dir}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "spike") {
		t.Fatalf("run err = %v, want one naming the uncovered package", err)
	}
}

func TestMeasureReportsAnUnreadableProfile(t *testing.T) {
	if _, err := measure("mode: atomic\nnot a profile entry\n", testModule, threeAreas); err == nil {
		t.Fatal("measure accepted a profile it could not parse")
	}
}

// TestBlocksReportsAScannerFailure covers the one read error a profile can
// produce on its own: a line past the buffer cap. It is reachable rather
// than theoretical — a profile is machine-written, so a corrupt one is
// likelier to be one enormous line than a malformed short one.
func TestBlocksReportsAScannerFailure(t *testing.T) {
	huge := "mode: atomic\n" + testModule + "/a/b.go:1.1,2.9 1 " + strings.Repeat("9", 5<<20) + "\n"
	_, err := blocks(huge)
	if err == nil || !strings.Contains(err.Error(), "read coverage profile") {
		t.Fatalf("blocks err = %v, want a read failure", err)
	}
}
