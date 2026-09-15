package docs_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// probaviPathPrefix marks a path an adapter invented for its own use. The
// project's own prefix is the reliable signal: an engine-owned path
// (/var/lib/mysql, /opt/mssql-tools18/bin/sqlcmd) is not one of these, and
// an adapter has no business writing anywhere else it did not derive.
const probaviPathPrefix = "/probavi"

// TestAdaptersComposeTheirWorkingPathsFromScratchDir refuses an adapter
// that roots its own working directory at /.
//
// §6.2 hands every provision request sandbox.scratch_dir, "a writable
// directory inside the sandbox guaranteed by the provider" — the one path
// an adapter may rely on. Four adapters did not use it for their data
// directories and composed absolute paths instead, and the docker provider
// hid it completely: commands run as root on a disposable filesystem, so a
// directory under / costs nothing. On the bare-host provider every payload
// runs as the drill user in a workspace it owns, / is not writable, and the
// drill dies before the restore — redis with "mkdir: cannot create
// directory '/probavi-redis': Permission denied" (issue #287).
// docs/sandbox-bare-host.md §4 already stated the rule from the other side.
//
// The test is the rule, not a list: a /probavi literal is allowed only
// where it is appended to something, because that something is the
// scratch directory the provider named. A literal standing on its own is
// a path the adapter chose for itself.
func TestAdaptersComposeTheirWorkingPathsFromScratchDir(t *testing.T) {
	files := adapterSources(t)
	if len(files) == 0 {
		t.Fatal("no adapter sources found — this gate would pass vacuously")
	}
	checked := 0
	for _, file := range files {
		rel, err := filepath.Rel(repoRoot, file)
		if err != nil {
			rel = file
		}
		for _, found := range rootedProbaviPaths(t, file) {
			checked++
			t.Errorf("%s:%d: %q is an absolute path this adapter chose for itself. "+
				"Compose it from the scratch_dir the provision request carries — that is the "+
				"only directory a provider guarantees, and / is not writable on a bare host",
				rel, found.line, found.path)
		}
	}
	if checked == 0 && !namesAnyProbaviPath(t, files) {
		t.Error("no adapter names a /probavi path at all — this gate is watching something that moved")
	}
}

// rootedPath is one offending literal and where it is.
type rootedPath struct {
	path string
	line int
}

// rootedProbaviPaths returns the /probavi literals in one file that stand
// on their own. A literal appended to something is derived from that
// something, which for these paths is the scratch directory; a literal
// standing alone is a path the adapter chose for itself.
func rootedProbaviPaths(t *testing.T, file string) []rootedPath {
	t.Helper()
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	appended := map[ast.Node]bool{}
	ast.Inspect(parsed, func(n ast.Node) bool {
		if bin, ok := n.(*ast.BinaryExpr); ok && bin.Op == token.ADD {
			appended[bin.Y] = true
		}
		return true
	})
	var found []rootedPath
	ast.Inspect(parsed, func(n ast.Node) bool {
		value, ok := probaviPathLiteral(n)
		if ok && !appended[n] {
			found = append(found, rootedPath{path: value, line: fset.Position(n.Pos()).Line})
		}
		return true
	})
	return found
}

// namesAnyProbaviPath reports whether any adapter still names a path in the
// project's own namespace. When none does, this gate has lost its subject
// and says so rather than passing on an empty set.
func namesAnyProbaviPath(t *testing.T, files []string) bool {
	t.Helper()
	for _, file := range files {
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		names := false
		ast.Inspect(parsed, func(n ast.Node) bool {
			if _, ok := probaviPathLiteral(n); ok {
				names = true
			}
			return !names
		})
		if names {
			return true
		}
	}
	return false
}

// probaviPathLiteral reports whether a node is a string literal naming a
// path in the project's own namespace, and returns its value.
func probaviPathLiteral(n ast.Node) (string, bool) {
	lit, ok := n.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(lit.Value)
	if err != nil || !strings.HasPrefix(value, probaviPathPrefix) {
		return "", false
	}
	return value, true
}

// adapterSources lists every non-test Go file under adapters/.
func adapterSources(t *testing.T) []string {
	t.Helper()
	var files []string
	entries, err := os.ReadDir(filepath.Join(repoRoot, "adapters"))
	if err != nil {
		t.Fatalf("read adapters: %v", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		matches, err := filepath.Glob(filepath.Join(repoRoot, "adapters", entry.Name(), "*.go"))
		if err != nil {
			t.Fatalf("glob %s: %v", entry.Name(), err)
		}
		for _, m := range matches {
			if !strings.HasSuffix(m, "_test.go") {
				files = append(files, m)
			}
		}
	}
	return files
}
