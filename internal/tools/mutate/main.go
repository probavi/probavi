// Command mutate measures what the tests would refuse, not what they ran.
//
// Coverage says a line executed. It cannot say whether anything would have
// failed had the line behaved differently, and the difference is not
// academic here: at 97% coverage, changing `len(sig) != ed25519.Signature
// Size` to `==` left every test in internal/evidence green, and so did
// replacing a drill's declared source parameters with empty ones on their
// way to the adapter. Both were found this way and are now asserted.
//
// The method is the standard one. For each declared package this makes one
// small change to a source file — a comparison swapped, a negation
// dropped, a 0 turned into a 1 — runs that package's own tests, and puts
// the file back. A change the tests notice is caught; one they do not is a
// survivor, and every survivor is either a missing assertion or a change
// with no observable effect.
//
// Survivors are budgeted rather than forbidden. Some changes genuinely
// cannot be observed — a value returned on an error path the caller
// ignores, a comparison the enclosing condition already settled, a guard
// the next line repeats — and a tool that demanded zero would be asking
// for tests that assert nothing. `.mutation-budget` holds one committed
// ceiling per package, lowered when the tests improve and never raised
// without saying why, the same ratchet `.coverage-floor` is.
//
// It edits files in place while it works, so it refuses to start on a
// package with uncommitted changes and restores the file on the way out,
// including on interrupt.
//
// It is a repository tool, not a shipped binary.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "mutate: %v\n", err)
		os.Exit(1)
	}
}

// budget is one declared package and the number of survivors it may have.
type budget struct {
	// Dir is the package directory, relative to the repository root.
	Dir string
	// Max is the largest number of surviving mutants the package may
	// carry before this tool fails.
	Max int
}

// verdict is what one mutant's test run said.
type verdict int

const (
	caught   verdict = iota // a test failed, or the run never finished
	survived                // every test passed with the code changed
	invalid                 // the change does not compile: no mutant at all
)

// tally is one package's run.
type tally struct {
	dir                       string
	caught, survived, invalid int
	survivors                 []string
}

func run(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("mutate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", ".", "repository root")
	budgets := fs.String("budgets", ".mutation-budget", "committed survivor ceilings, one package per line")
	only := fs.String("only", "", "run just this declared package")
	timeout := fs.Duration("timeout", 2*time.Minute, "bound on one package's test run; a run that exceeds it counts as caught")
	if err := fs.Parse(args); err != nil {
		return err
	}
	declared, err := readBudgets(filepath.Join(*root, *budgets))
	if err != nil {
		return err
	}
	if *only != "" {
		declared = slices.DeleteFunc(declared, func(b budget) bool { return b.Dir != *only })
		if len(declared) == 0 {
			return fmt.Errorf("%s declares no package %q", *budgets, *only)
		}
	}

	over := []string{}
	for _, b := range declared {
		t, err := mutatePackage(*root, b.Dir, *timeout, stderr)
		if err != nil {
			return err
		}
		report(stdout, t, b.Max)
		if t.survived > b.Max {
			over = append(over, fmt.Sprintf("%s: %d survivors, budget %d", b.Dir, t.survived, b.Max))
		}
	}
	if len(over) > 0 {
		return fmt.Errorf("a change the tests do not notice is a missing assertion or an unobservable change; "+
			"assert it or say why it cannot be asserted, then move the budget down — %s",
			strings.Join(over, "; "))
	}
	return nil
}

// readBudgets parses the committed ceilings. Every package this tool runs
// is declared there, so a package joins the run with a decision about its
// budget attached rather than joining it silently.
func readBudgets(path string) ([]budget, error) {
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("read budgets: %w", err)
	}
	var out []budget
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		fields := strings.Fields(text)
		if len(fields) != 2 {
			return nil, fmt.Errorf("%s line %d is not \"<package dir> <max survivors>\": %q", path, line, text)
		}
		max, err := strconv.Atoi(fields[1])
		if err != nil || max < 0 {
			return nil, fmt.Errorf("%s line %d has an unreadable ceiling: %q", path, line, fields[1])
		}
		out = append(out, budget{Dir: filepath.ToSlash(fields[0]), Max: max})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read budgets: %w", err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s declares no packages; this run would pass vacuously", path)
	}
	return out, nil
}

// edit is one mutation: a byte range of a source file and what replaces it.
type edit struct {
	file     string
	offset   int
	old, new string
	line     int
}

// relativeTo names the file as the repository does, so a survivor reads
// the way a reviewer would cite it.
func (e edit) relativeTo(root string) edit {
	abs, err := filepath.Abs(root)
	if err != nil {
		return e
	}
	rel, err := filepath.Rel(abs, e.file)
	if err != nil {
		return e
	}
	e.file = filepath.ToSlash(rel)
	return e
}

func (e edit) String() string {
	shown := e.new
	if shown == "" {
		shown = "(removed)"
	}
	return fmt.Sprintf("%s:%d  %q -> %s", e.file, e.line, e.old, shown)
}

// swaps are the comparisons and connectives this tool exchanges. The set
// is deliberately small: each one produces a program a reviewer can hold
// in their head, which is what makes a survivor worth reading.
var swaps = map[token.Token]token.Token{
	token.EQL: token.NEQ, token.NEQ: token.EQL,
	token.LSS: token.LEQ, token.LEQ: token.LSS,
	token.GTR: token.GEQ, token.GEQ: token.GTR,
	token.LAND: token.LOR, token.LOR: token.LAND,
}

// collect finds every mutation one file offers. Edits are byte splices
// rather than a reprinted syntax tree, so a mutant differs from the
// original in exactly the bytes named here.
func collect(path string, src []byte) ([]edit, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var edits []edit
	ast.Inspect(f, func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.BinaryExpr:
			if to, ok := swaps[e.Op]; ok {
				p := fset.Position(e.OpPos)
				edits = append(edits, edit{path, p.Offset, e.Op.String(), to.String(), p.Line})
			}
		case *ast.UnaryExpr:
			if e.Op == token.NOT {
				p := fset.Position(e.OpPos)
				edits = append(edits, edit{path, p.Offset, "!", "", p.Line})
			}
		case *ast.BasicLit:
			// 0 and 1 are where the boundaries live: an empty slice, a
			// first element, a budget of none.
			if e.Kind == token.INT && (e.Value == "0" || e.Value == "1") {
				p := fset.Position(e.ValuePos)
				to := "1"
				if e.Value == "1" {
					to = "0"
				}
				edits = append(edits, edit{path, p.Offset, e.Value, to, p.Line})
			}
		}
		return true
	})
	return edits, nil
}

// apply splices one edit into a file's bytes.
func apply(src []byte, e edit) []byte {
	out := make([]byte, 0, len(src)+len(e.new))
	out = append(out, src[:e.offset]...)
	out = append(out, e.new...)
	return append(out, src[e.offset+len(e.old):]...)
}

// mutatePackage runs every mutant of one package's non-test sources.
func mutatePackage(repoRoot, dir string, timeout time.Duration, stderr io.Writer) (t tally, err error) {
	pkgDir, err := filepath.Abs(filepath.Join(repoRoot, filepath.FromSlash(dir)))
	if err != nil {
		return tally{}, fmt.Errorf("resolve %s: %w", dir, err)
	}
	if err := requireClean(pkgDir); err != nil {
		return tally{}, err
	}
	module, err := moduleRoot(pkgDir)
	if err != nil {
		return tally{}, err
	}
	pattern, err := filepath.Rel(module, pkgDir)
	if err != nil {
		return tally{}, fmt.Errorf("locate %s inside its module: %w", dir, err)
	}

	edits, err := collectDir(pkgDir)
	if err != nil {
		return tally{}, err
	}
	fmt.Fprintf(stderr, "%s: %d mutants\n", dir, len(edits))

	root, err := os.OpenRoot(pkgDir)
	if err != nil {
		return tally{}, fmt.Errorf("open %s: %w", dir, err)
	}
	defer func() {
		if cerr := root.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close %s: %w", dir, cerr)
		}
	}()

	t = tally{dir: dir}
	for _, e := range edits {
		v, verr := runMutant(root, module, "./"+filepath.ToSlash(pattern), e, timeout)
		if verr != nil {
			return tally{}, verr
		}
		switch v {
		case caught:
			t.caught++
		case invalid:
			t.invalid++
		case survived:
			t.survived++
			t.survivors = append(t.survivors, e.relativeTo(repoRoot).String())
		}
	}
	return t, nil
}

// collectDir gathers the mutations of every non-test source file in dir.
func collectDir(dir string) ([]edit, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var edits []edit
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		src, err := os.ReadFile(filepath.Clean(path))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		found, err := collect(path, src)
		if err != nil {
			return nil, err
		}
		edits = append(edits, found...)
	}
	sort.Slice(edits, func(i, j int) bool {
		if edits[i].file != edits[j].file {
			return edits[i].file < edits[j].file
		}
		return edits[i].offset < edits[j].offset
	})
	return edits, nil
}

// runMutant writes one mutant, runs the package's tests, and puts the file
// back — including when the run is interrupted, because a tool that leaves
// a mutated source behind is worse than no tool.
func runMutant(root *os.Root, module, pattern string, e edit, timeout time.Duration) (v verdict, err error) {
	// Every write goes through the root, which is the package directory
	// itself: the tool cannot write outside it even if a name it was
	// handed tried to, and the name is checked before it is used.
	name, err := within(root.Name(), e.file)
	if err != nil {
		return caught, err
	}
	src, err := os.ReadFile(filepath.Join(root.Name(), name))
	if err != nil {
		return caught, fmt.Errorf("read %s: %w", e.file, err)
	}
	restore := func() error {
		if werr := root.WriteFile(name, src, 0o600); werr != nil {
			return fmt.Errorf("restore %s: %w", e.file, werr)
		}
		return nil
	}
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		select {
		case <-interrupt:
			_ = restore() //nolint:errcheck // the process is going down; the write is the last useful act
			os.Exit(1)
		case <-done:
		}
	}()
	defer func() {
		close(done)
		signal.Stop(interrupt)
		if rerr := restore(); rerr != nil && err == nil {
			err = rerr
		}
	}()

	if werr := root.WriteFile(name, apply(src, e), 0o600); werr != nil {
		return caught, fmt.Errorf("write mutant into %s: %w", e.file, werr)
	}
	return testVerdict(module, pattern, timeout), nil
}

// within returns a file's name relative to dir, and refuses one that
// reaches outside it. This tool writes to what it is given, so where it
// may write is stated rather than assumed — the root below enforces the
// same rule at the system call.
func within(dir, file string) (string, error) {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(file))
	if err != nil {
		return "", fmt.Errorf("locate %s inside %s: %w", file, dir, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside the package directory %s", file, dir)
	}
	return rel, nil
}

// testVerdict runs one package's tests over the mutant in place.
//
// A run that does not finish is caught rather than hung over: a change
// that turns a bounded walk into an endless one is exactly the kind the
// tests must not accept, and `go test -timeout` fails the run itself.
func testVerdict(module, pattern string, timeout time.Duration) verdict {
	cmd := exec.Command("go", "test", "-count=1", "-timeout", timeout.String(), pattern)
	cmd.Dir = module
	out, err := cmd.CombinedOutput()
	if err == nil {
		return survived
	}
	if didNotCompile(string(out)) {
		return invalid
	}
	return caught
}

// didNotCompile reports whether the toolchain refused the mutant. Such a
// change is no mutant at all: nothing was tested, and counting it either
// way would misstate what the suite does.
func didNotCompile(output string) bool {
	for _, marker := range []string{
		"[build failed]", "build failed", "cannot use", "declared and not used",
		"undefined:", "mismatched types", "invalid operation", "syntax error",
	} {
		if strings.Contains(output, marker) {
			return true
		}
	}
	return false
}

// requireClean refuses to mutate a package with uncommitted changes: this
// tool edits files in place, and a crash between the write and the restore
// must never be able to cost somebody work they had not committed.
func requireClean(dir string) error {
	cmd := exec.Command("git", "status", "--porcelain", "--", ".")
	// Asked from inside the package, so the answer comes from whichever
	// repository owns it — this one, or a fixture a test built.
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("check %s for uncommitted changes: %w", dir, err)
	}
	if strings.TrimSpace(string(out)) != "" {
		return fmt.Errorf("%s has uncommitted changes; this tool edits files in place, so commit or stash first:\n%s",
			dir, strings.TrimRight(string(out), "\n"))
	}
	return nil
}

// moduleRoot walks up from dir to the module that owns it. The repository
// has two — the core and the independent verifier — and a package is run
// from its own.
func moduleRoot(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", dir, err)
	}
	for cur := abs; ; {
		if _, err := os.Stat(filepath.Join(cur, "go.mod")); err == nil {
			return cur, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("look for the module of %s: %w", dir, err)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", fmt.Errorf("%s belongs to no module", dir)
		}
		cur = parent
	}
}

// report prints one package's run and the survivors by name: the list is
// the useful half, because each line names a change nothing refused.
func report(w io.Writer, t tally, max int) {
	fmt.Fprintf(w, "%-28s %3d caught, %3d survived (budget %d), %3d did not compile\n",
		t.dir, t.caught, t.survived, max, t.invalid)
	for _, s := range t.survivors {
		fmt.Fprintf(w, "    survived: %s\n", s)
	}
}
