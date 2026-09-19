package main

import (
	"go/parser"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadBudgets(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	good := write("good", "# ceilings\n\ninternal/evidence 15\ninternal/adapter  15\n")
	got, err := readBudgets(good)
	if err != nil {
		t.Fatalf("readBudgets: %v", err)
	}
	want := []budget{{"internal/evidence", 15}, {"internal/adapter", 15}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("budgets = %+v, want %+v", got, want)
	}

	for name, content := range map[string]string{
		"a line that is not a pair":      "internal/evidence\n",
		"a ceiling that is not a number": "internal/evidence many\n",
		"a negative ceiling":             "internal/evidence -1\n",
		// A file declaring nothing would make every run pass without
		// mutating anything, which is worse than no run at all.
		"nothing declared": "# only a comment\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := readBudgets(write("bad", content)); err == nil {
				t.Errorf("readBudgets accepted %q", content)
			}
		})
	}
	if _, err := readBudgets(filepath.Join(dir, "absent")); err == nil {
		t.Error("readBudgets accepted a missing file")
	}
}

const sample = `package p

func f(a, b int, s []string, ok bool) int {
	if a == b && len(s) > 0 {
		return 1
	}
	if !ok || a < b {
		return 0
	}
	return 2
}
`

func TestCollectFindsTheChangesWorthMaking(t *testing.T) {
	edits, err := collect("sample.go", []byte(sample))
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	got := map[string]int{}
	for _, e := range edits {
		got[e.old+"->"+e.new]++
	}
	want := map[string]int{
		"==->!=": 1, "&&->||": 1, ">->>=": 1, "!->": 1, "||->&&": 1, "<-><=": 1,
		"0->1": 2, "1->0": 1,
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("collect found %d of %q, want %d (all: %v)", got[k], k, n, got)
		}
	}
	// 2 is neither a boundary nor a comparison: leaving it alone is what
	// keeps a survivor list worth reading.
	if got["2->1"] != 0 || got["2->3"] != 0 {
		t.Errorf("collect mutated a literal that marks no boundary: %v", got)
	}
}

func TestCollectRefusesWhatItCannotParse(t *testing.T) {
	if _, err := collect("broken.go", []byte("package p\nfunc (")); err == nil {
		t.Error("collect parsed a file that is not Go")
	}
}

// TestApplyChangesExactlyTheNamedBytes: a mutant differs from the original
// in the operator and nothing else — no reprinting, no reformatting — so a
// survivor is a change a reader can hold in their head.
func TestApplyChangesExactlyTheNamedBytes(t *testing.T) {
	src := []byte(sample)
	edits, err := collect("sample.go", src)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, e := range edits {
		got := string(apply(src, e))
		if len(got) != len(src)-len(e.old)+len(e.new) {
			t.Errorf("%s changed the file's length by more than the edit", e)
		}
		if got == string(src) {
			t.Errorf("%s changed nothing", e)
		}
		// Everything before and after the splice is byte-identical.
		if got[:e.offset] != string(src[:e.offset]) ||
			got[e.offset+len(e.new):] != string(src[e.offset+len(e.old):]) {
			t.Errorf("%s disturbed bytes outside its own span", e)
		}
	}
}

func TestDidNotCompileReadsTheToolchain(t *testing.T) {
	for name, tc := range map[string]struct {
		output string
		want   bool
	}{
		"a build failure":      {"# probavi/pkg [build failed]\n", true},
		"a type error":         {"./x.go:3:9: invalid operation: a < b\n", true},
		"an undefined symbol":  {"./x.go:3:9: undefined: foo\n", true},
		"a failing test":       {"--- FAIL: TestX (0.00s)\nFAIL\n", false},
		"a run that timed out": {"panic: test timed out after 2m0s\n", false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := didNotCompile(tc.output); got != tc.want {
				t.Errorf("didNotCompile(%q) = %v, want %v", tc.output, got, tc.want)
			}
		})
	}
}

// TestModuleRootFindsTheOwningModule: this repository has two modules, and
// a package's tests run from its own.
func TestModuleRootFindsTheOwningModule(t *testing.T) {
	root := t.TempDir()
	inner := filepath.Join(root, "spec", "evidence")
	deep := filepath.Join(inner, "internal", "x")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{root, inner} {
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module m\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := moduleRoot(deep)
	if err != nil {
		t.Fatalf("moduleRoot: %v", err)
	}
	if got != inner {
		t.Errorf("moduleRoot = %s, want the nearest module %s", got, inner)
	}
}

func TestModuleRootRefusesAPathOutsideAnyModule(t *testing.T) {
	dir := t.TempDir()
	if _, err := moduleRoot(dir); err == nil {
		t.Skip("the temporary directory sits inside a module on this machine")
	}
}

// TestReportNamesEverySurvivor: the count says how the package did; the
// list is what somebody acts on.
func TestReportNamesEverySurvivor(t *testing.T) {
	var out strings.Builder
	report(&out, tally{
		dir: "internal/evidence", caught: 225, survived: 2, invalid: 2,
		survivors: []string{`internal/evidence/store.go:88  "&&" -> ||`, `internal/evidence/keys.go:46  "0" -> 1`},
	}, 15)
	text := out.String()
	for _, want := range []string{"internal/evidence", "225 caught", "2 survived", "budget 15",
		"store.go:88", "keys.go:46"} {
		if !strings.Contains(text, want) {
			t.Errorf("report = %q, want it to carry %q", text, want)
		}
	}
}

// TestRunRefusesABudgetFileItCannotRead keeps the failure a clear one: a
// run that cannot read its ceilings has measured nothing.
func TestRunRefusesABudgetFileItCannotRead(t *testing.T) {
	var out, errOut strings.Builder
	err := run([]string{"-root", t.TempDir()}, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "read budgets") {
		t.Errorf("run = %v, want a refusal naming the budgets file", err)
	}
}

// TestRunRefusesAPackageItWasNotToldAbout: -only names a declared package,
// so a typo fails rather than quietly running everything.
func TestRunRefusesAPackageItWasNotToldAbout(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".mutation-budget"),
		[]byte("internal/evidence 15\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut strings.Builder
	err := run([]string{"-root", root, "-only", "internal/adaptor"}, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "internal/adaptor") {
		t.Errorf("run = %v, want a refusal naming the package", err)
	}
}

// fixture shapes the module a test mutates.
type fixture struct {
	// extra files are written into the module, overriding the defaults.
	extra map[string]string
	// noModule leaves out go.mod; noGit leaves the tree out of a
	// repository. Each drives one refusal.
	noModule, noGit bool
	// budget is the .mutation-budget content; empty means "answered 1".
	budget string
}

// fixtureModule builds a module the tool can mutate for real: one package
// whose behaviour a test asserts, one whose behaviour nothing does, and a
// git repository around them, because the tool refuses to edit a tree
// with uncommitted work in it.
func fixtureModule(t *testing.T, fx fixture) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "answered"), 0o755); err != nil {
		t.Fatal(err)
	}
	budget := fx.budget
	if budget == "" {
		budget = "answered 1\n"
	}
	files := map[string]string{
		"go.mod": "module fixture\n\ngo 1.25\n",
		// `>=` is asserted by the test below; `!` is not, so exactly one
		// mutant of this package survives.
		"answered/answered.go": `package answered

func AtLeast(n, min int) bool {
	return n >= min
}

func Unasserted(ok bool) string {
	if !ok {
		return "no"
	}
	return "yes"
}
`,
		"answered/answered_test.go": `package answered

import "testing"

func TestAtLeast(t *testing.T) {
	for _, tc := range []struct {
		n, min int
		want   bool
	}{{1, 2, false}, {2, 2, true}, {3, 2, true}} {
		if got := AtLeast(tc.n, tc.min); got != tc.want {
			t.Errorf("AtLeast(%d, %d) = %v, want %v", tc.n, tc.min, got, tc.want)
		}
	}
}
`,
		".mutation-budget": budget,
	}
	if fx.noModule {
		delete(files, "go.mod")
	}
	for name, content := range fx.extra {
		files[name] = content
	}
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if fx.noGit {
		return root
	}
	for _, args := range [][]string{
		{"init", "-q"}, {"config", "user.email", "fixture@example.invalid"},
		{"config", "user.name", "fixture"}, {"add", "-A"},
		{"-c", "commit.gpgsign=false", "commit", "-q", "-m", "fixture"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git %v in the fixture: %v: %s", args, err, out)
		}
	}
	return root
}

// TestRunMutatesForReal drives the whole tool against that module: the
// asserted comparison is caught, the unasserted negation survives and is
// named, the budget decides the verdict, and the sources come back
// byte-for-byte.
func TestRunMutatesForReal(t *testing.T) {
	root := fixtureModule(t, fixture{})
	source := filepath.Join(root, "answered", "answered.go")
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}

	var out, errOut strings.Builder
	if err := run([]string{"-root", root, "-timeout", "60s"}, &out, &errOut); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	text := out.String()
	if !strings.Contains(text, "1 survived (budget 1)") {
		t.Errorf("report = %q, want one survivor against the budget", text)
	}
	if !strings.Contains(text, `answered/answered.go`) || !strings.Contains(text, `"!"`) {
		t.Errorf("report = %q, want the surviving negation named", text)
	}

	after, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("the source did not come back as it was")
	}

	// The same run against a ceiling of none is a failure, and the message
	// says what to do about it.
	if err := os.WriteFile(filepath.Join(root, ".mutation-budget"), []byte("answered 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	err = run([]string{"-root", root, "-timeout", "60s"}, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "1 survivors, budget 0") {
		t.Errorf("run = %v, want the package named over its budget", err)
	}
}

// TestRunRefusesUncommittedWork: the tool edits sources in place, so work
// nobody has committed must never be at risk.
func TestRunRefusesUncommittedWork(t *testing.T) {
	root := fixtureModule(t, fixture{})
	source := filepath.Join(root, "answered", "answered.go")
	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, append(raw, []byte("\n// work in progress\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut strings.Builder
	err = run([]string{"-root", root}, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
		t.Errorf("run = %v, want a refusal naming the uncommitted work", err)
	}
}

func TestRunRefusesFlagsItDoesNotHave(t *testing.T) {
	var out, errOut strings.Builder
	if err := run([]string{"-nope"}, &out, &errOut); err == nil {
		t.Error("run accepted a flag it does not define")
	}
}

// TestAChangeThatDoesNotCompileIsNoMutant: the toolchain refusing a
// change means nothing was tested, and counting it as caught would say
// the suite noticed something it never ran.
func TestAChangeThatDoesNotCompileIsNoMutant(t *testing.T) {
	root := fixtureModule(t, fixture{extra: map[string]string{
		// One element, so turning the index 0 into 1 is a compile error
		// rather than a mutant.
		"answered/bounds.go": `package answered

func First() int {
	a := [1]int{7}
	return a[0]
}
`,
	}})
	var out, errOut strings.Builder
	if err := run([]string{"-root", root, "-timeout", "60s"}, &out, &errOut); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	// Both literals in the fixture break the build: the array's length and
	// its index. What matters is that neither was counted as tested.
	if !strings.Contains(out.String(), "2 did not compile") {
		t.Errorf("report = %q, want the uncompilable changes counted apart", out.String())
	}
}

// TestRunRefusesWhatItCannotMutate covers the three refusals that come
// before any test runs: a tree no repository owns, a package inside no
// module, and a source file that is not Go.
func TestRunRefusesWhatItCannotMutate(t *testing.T) {
	for name, tc := range map[string]struct {
		fx   fixture
		want string
	}{
		"a tree outside any repository": {fixture{noGit: true}, "uncommitted changes"},
		"a package inside no module":    {fixture{noModule: true}, "belongs to no module"},
		"a file that is not Go": {
			fixture{extra: map[string]string{"answered/broken.go": "package answered\nfunc ("}},
			"parse",
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := fixtureModule(t, tc.fx)
			var out, errOut strings.Builder
			err := run([]string{"-root", root, "-timeout", "60s"}, &out, &errOut)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("run = %v, want a refusal mentioning %q", err, tc.want)
			}
		})
	}
}

// TestASourceTheToolCannotWriteStopsTheRun: the mutant is written into
// the file itself, so a source it cannot write is a failure to report
// rather than a package quietly reported as fully caught.
func TestASourceTheToolCannotWriteStopsTheRun(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes a read-only file")
	}
	root := fixtureModule(t, fixture{})
	source := filepath.Join(root, "answered", "answered.go")
	if err := os.Chmod(source, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(source, 0o600); err != nil {
			t.Errorf("restore the mode: %v", err)
		}
	})
	var out, errOut strings.Builder
	err := run([]string{"-root", root, "-timeout", "60s"}, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "answered.go") {
		t.Errorf("run = %v, want a refusal naming the file it could not write", err)
	}
}

// TestASourceTheToolCannotReadStopsTheRun: the same rule one step
// earlier, where the file is read to find its mutations.
func TestASourceTheToolCannotReadStopsTheRun(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 file")
	}
	root := fixtureModule(t, fixture{})
	source := filepath.Join(root, "answered", "answered.go")
	if err := os.Chmod(source, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(source, 0o600); err != nil {
			t.Errorf("restore the mode: %v", err)
		}
	})
	var out, errOut strings.Builder
	err := run([]string{"-root", root, "-timeout", "60s"}, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "answered.go") {
		t.Errorf("run = %v, want a refusal naming the file it could not read", err)
	}
}

// TestWithinRefusesAPathOutsideThePackage: this tool writes to the file
// it is handed, so where it may write is asserted rather than assumed —
// and what it answers is the name inside the package, which is what the
// root it writes through accepts.
func TestWithinRefusesAPathOutsideThePackage(t *testing.T) {
	dir := filepath.Join("repo", "internal", "evidence")
	for name, tc := range map[string]struct {
		file string
		want string
	}{
		"a file in the package":    {filepath.Join(dir, "store.go"), "store.go"},
		"a file below the package": {filepath.Join(dir, "sub", "x.go"), filepath.Join("sub", "x.go")},
		"a sibling package":        {filepath.Join("repo", "internal", "adapter", "x.go"), ""},
		"a path climbing out":      {filepath.Join(dir, "..", "..", "..", "etc", "passwd"), ""},
		"the parent itself":        {filepath.Join("repo", "internal"), ""},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := within(dir, tc.file)
			if tc.want == "" {
				if err == nil {
					t.Errorf("within accepted %q (as %q)", tc.file, got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("within = %q, %v, want %q", got, err, tc.want)
			}
		})
	}
}

// TestRunMutantRefusesAFileOutsideTheRoot: the containment check is not
// decoration. It runs before the file is read, so a name that reaches out
// of the package is refused rather than written to.
func TestRunMutantRefusesAFileOutsideTheRoot(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Errorf("close root: %v", err)
		}
	})
	outside := filepath.Join(dir, "..", "elsewhere.go")
	_, err = runMutant(root, dir, "./...", edit{file: outside, old: "<", new: "<="}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "outside the package directory") {
		t.Errorf("runMutant = %v, want a refusal naming the containment", err)
	}
}

// TestTheZeroBesideARefusalIsLeftAlone: Go returns a zero with an error
// because it must, and every caller reads the error instead. Mutating
// that zero produces a survivor nobody can act on, so it is not produced
// at all — while a zero that is an answer stays mutable.
func TestTheZeroBesideARefusalIsLeftAlone(t *testing.T) {
	const src = `package p

import (
	"errors"
	"fmt"
)

func built() (int, error) {
	return 0, fmt.Errorf("no")
}

func sentinel() (int, error) {
	return 0, errors.New("no")
}

func named() (int, error) {
	malformed := fmt.Errorf("no")
	if true {
		return 0, malformed
	}
	return 0, nil
}

func passedOn() (int, error) {
	v, err := built()
	if err != nil {
		return 0, err
	}
	return v, nil
}

func reported() (int, bool) {
	return 0, false
}

func counted() (int, error) {
	// A one beside a refusal still means something, and so does a zero
	// returned with no refusal at all.
	if false {
		return 1, errors.New("no")
	}
	return 0, nil
}
`
	edits, err := collect("sample.go", []byte(src))
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	zeros := 0
	ones := 0
	for _, e := range edits {
		switch e.old {
		case "0":
			zeros++
		case "1":
			ones++
		}
	}
	// The zeros left mutable are the two returned beside a nil error, in
	// named() and counted(); every other zero accompanies a refusal.
	if zeros != 2 {
		t.Errorf("collect kept %d zeros, want the two that are answers: %v", zeros, edits)
	}
	if ones != 1 {
		t.Errorf("collect kept %d ones, want the one beside a refusal: %v", ones, edits)
	}
}

// TestARefusalIsRecognisedByShapeNotByLuck pins the rule itself, because
// it decides what never reaches a survivor list.
func TestARefusalIsRecognisedByShapeNotByLuck(t *testing.T) {
	for name, tc := range map[string]struct {
		expr string
		want bool
	}{
		"fmt.Errorf":        {`fmt.Errorf("x")`, true},
		"errors.New":        {`errors.New("x")`, true},
		"errors.Join":       {`errors.Join(a, b)`, true},
		"err":               {`err`, true},
		"a suffixed error":  {`werr`, true},
		"an exported error": {`readErr`, true},
		"false":             {`false`, true},
		"true":              {`true`, false},
		"nil":               {`nil`, false},
		"a plain value":     {`count`, false},
		"another call":      {`compute()`, false},
		"a package call":    {`json.Marshal(v)`, false},
	} {
		t.Run(name, func(t *testing.T) {
			expr, err := parser.ParseExpr(tc.expr)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.expr, err)
			}
			if got := isRefusal(expr, map[string]bool{}); got != tc.want {
				t.Errorf("isRefusal(%s) = %v, want %v", tc.expr, got, tc.want)
			}
		})
	}
}
