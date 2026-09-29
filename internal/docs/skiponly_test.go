package docs_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A test that skips on a false answer cannot tell "this platform cannot do
// that" from "the thing under test stopped working". Three tests in this
// repository were found doing it, and one of them — the whole body of which
// was a skip guard calling the function it was named for — skipped when
// that function answered and passed without a single check when it did not.
//
// That narrowest shape is the one a machine can find: a test function that
// reaches a Skip and never reaches anything that could fail it. This gate
// finds it. What it deliberately does not find is the other two shapes,
// because neither is mechanically distinguishable from a legitimate
// precondition:
//
//   - a skip whose condition calls the function under test, where the rest
//     of the function does assert something;
//   - a skip sitting in front of an assertion it makes unreachable.
//
// Both of those took reading, and this gate does not pretend otherwise. It
// closes the class that can be closed.

// assertionPrefixes are the names this repository gives helpers that can
// fail a test on the caller's behalf. A test asserting only through one of
// them is asserting, and the convention is consistent enough to lean on:
// assertArgs, assertOutcome, assertPassRecord, mustLoad, mustReject.
//
// A helper named outside the convention makes this gate report its caller,
// and the two ways out are both improvements — rename the helper, or assert
// directly in the test that owns the claim.
var assertionPrefixes = []string{"assert", "must", "require", "want"}

// skipCalls and failCalls are the method names that decide the verdict.
// Skip is how a test declines to answer; the rest are how it answers no.
var (
	skipCalls = map[string]bool{"Skip": true, "Skipf": true, "SkipNow": true}
	failCalls = map[string]bool{
		"Error": true, "Errorf": true, "Fatal": true, "Fatalf": true,
		"Fail": true, "FailNow": true,
	}
)

// skipOnlyTests returns the test functions in one parsed file that reach a
// Skip and nothing that could fail them.
func skipOnlyTests(filename string, src []byte) ([]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), filename, src, 0)
	if err != nil {
		return nil, err
	}
	var found []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil || fn.Recv != nil || !isTestName(fn.Name.Name) {
			continue
		}
		skips, asserts := walkCalls(fn.Body)
		if skips && !asserts {
			found = append(found, fn.Name.Name)
		}
	}
	return found, nil
}

// walkCalls reports whether a body reaches a skip and whether it reaches
// anything that could fail. Closures count: a subtest that asserts is the
// function asserting.
func walkCalls(body *ast.BlockStmt) (skips, asserts bool) {
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			switch {
			case skipCalls[fn.Sel.Name]:
				skips = true
			case failCalls[fn.Sel.Name]:
				asserts = true
			}
		case *ast.Ident:
			if hasAssertionPrefix(fn.Name) {
				asserts = true
			}
		}
		return true
	})
	return skips, asserts
}

func hasAssertionPrefix(name string) bool {
	lower := strings.ToLower(name)
	for _, p := range assertionPrefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

func isTestName(name string) bool {
	for _, prefix := range []string{"Test", "Fuzz", "Benchmark", "Example"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// TestNoTestAssertsNothingButASkip walks both modules' test files.
func TestNoTestAssertsNothingButASkip(t *testing.T) {
	var scanned int
	for _, file := range scannableFiles(t) {
		if !strings.HasSuffix(file, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(file)))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		names, err := skipOnlyTests(file, raw)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		scanned++
		for _, name := range names {
			t.Errorf("%s: %s reaches a Skip and nothing that could fail it, so it reports "+
				"as tested whatever the code does.\n"+
				"    Establish the precondition by some means the code under test does not decide — "+
				"looking at the platform, the environment, or the filesystem — and assert the rest.",
				file, name)
		}
	}
	// A gate that scanned nothing would pass for the wrong reason.
	if scanned < 50 {
		t.Fatalf("scanned %d test files, want the whole tree", scanned)
	}
}

// TestTheSkipOnlyDetectorSeesTheShapeAndSparesTheRest ties the rule to what
// it is for. A gate nobody has watched fail is a gate nobody knows the shape
// of, and the first case below is the one this repository actually carried.
func TestTheSkipOnlyDetectorSeesTheShapeAndSparesTheRest(t *testing.T) {
	for name, tc := range map[string]struct {
		src  string
		want bool
	}{
		"the shape this gate exists for": {`package p
import "testing"
func TestRefusesAPathOutsideAnyModule(t *testing.T) {
	dir := t.TempDir()
	if _, err := moduleRoot(dir); err == nil {
		t.Skip("the temporary directory sits inside a module on this machine")
	}
}`, true},
		"a skip in front of an assertion": {`package p
import "testing"
func TestSomething(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	if got := f(); got != 1 {
		t.Errorf("got %d", got)
	}
}`, false},
		"a skip in front of a helper that asserts": {`package p
import "testing"
func TestSomething(t *testing.T) {
	if !available() {
		t.Skip("not here")
	}
	assertOutcome(t, f())
}`, false},
		"a subtest that asserts": {`package p
import "testing"
func TestSomething(t *testing.T) {
	t.Run("one", func(t *testing.T) {
		if !available() {
			t.Skip("not here")
		}
		t.Fatal("no")
	})
}`, false},
		"no skip at all": {`package p
import "testing"
func TestSomething(t *testing.T) {
	if f() != 1 {
		t.Error("no")
	}
}`, false},
		"a fuzz target that only skips": {`package p
import "testing"
func FuzzSomething(f *testing.F) {
	if !available() {
		f.Skip("not here")
	}
}`, true},
		"not a test function": {`package p
import "testing"
func helper(t *testing.T) {
	t.Skip("this is a helper, and skipping is what it is for")
}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			names, err := skipOnlyTests("x_test.go", []byte(tc.src))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := len(names) > 0; got != tc.want {
				t.Errorf("flagged = %v (%v), want %v", got, names, tc.want)
			}
		})
	}
}
