package docs_test

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// ignoreFile is the repository's only ignore file. Both Go modules live
// under it: spec/evidence is a separate module but not a separate
// repository, so its build output is covered here too.
const ignoreFile = ".gitignore"

// TestEveryBuiltBinaryIsIgnored holds .gitignore to the tree rather than to
// whoever last remembered to edit it.
//
// `go build ./<dir>` writes an executable into the current directory named
// after the package directory, and `go build .` from inside that directory
// writes it there. Both land in the work tree next to tracked files, where
// `git add -A` picks them up: a 4.5 MB adapter binary reached version
// control exactly that way, because the ignore list was a hand-kept list of
// names and one adapter's name had never been added to it. Adding the name
// closed that instance; this closes the class.
//
// The first place is the *module* root, not the repository root: a path in
// another module is not in `./...` and `go build` refuses it outright, so
// spec/evidence's verifier can only be built from inside spec/evidence.
//
// A package whose binary name is already taken by a directory there is
// exempt from that half — `go build` refuses again ("build output ...
// already exists and is a directory"), so there is nothing to ignore, and
// an entry would instead hide new files added to that directory.
func TestEveryBuiltBinaryIsIgnored(t *testing.T) {
	patterns := ignorePatterns(t)
	for _, dir := range mainPackages(t) {
		name := path.Base(dir)
		if inPlace := path.Join(dir, name); !ignoredBy(patterns, inPlace) {
			t.Errorf("`go build .` inside %s drops %s, which %s does not ignore — add /%s",
				dir, inPlace, ignoreFile, inPlace)
		}
		module := moduleRoot(t, dir)
		atModuleRoot := path.Join(module, name)
		if info, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(atModuleRoot))); err == nil && info.IsDir() {
			continue
		}
		if !ignoredBy(patterns, atModuleRoot) {
			t.Errorf("`go build ./%s` from %s drops %s, which %s does not ignore — add /%s",
				strings.TrimPrefix(strings.TrimPrefix(dir, module), "/"), moduleLabel(module), atModuleRoot, ignoreFile, atModuleRoot)
		}
	}
}

// TestMainPackageScanFindsTheAdapters guards the guard: a walk that
// silently matched nothing would make the test above pass by finding no
// package to check. Every adapter directory is a main package, so the two
// lists must agree.
func TestMainPackageScanFindsTheAdapters(t *testing.T) {
	found := mainPackages(t)
	for _, adapter := range adapterDirs(t) {
		if dir := path.Join("adapters", adapter); !slices.Contains(found, dir) {
			t.Errorf("the main-package scan missed %s; it found %v", dir, found)
		}
	}
}

// TestIgnoredByReadsTheSyntaxThisFileUses pins the matcher against the
// pattern shapes .gitignore actually contains. The gate above is only worth
// what this is: a matcher that answered "ignored" to everything would pass
// it while the file said nothing at all.
func TestIgnoredByReadsTheSyntaxThisFileUses(t *testing.T) {
	patterns := []string{"*.exe", "/probavi", "/adapters/*/postgres", "/dist/", "*.jsonl", "!/docs/schemas/evidence/examples/*.jsonl"}
	for _, tc := range []struct {
		name string
		file string
		want bool
	}{
		{"unanchored pattern matches at any depth", "adapters/mssql/probavi.exe", true},
		{"anchored pattern matches only at the root", "probavi", true},
		{"anchored pattern does not match deeper", "cmd/probavi/probavi", false},
		{"a star matches inside one segment", "adapters/postgres/postgres", true},
		{"a star does not cross a separator", "adapters/a/b/postgres", false},
		{"an unlisted name is not ignored", "enginetable", false},
		{"a directory-only pattern does not match a file", "dist", false},
		{"a later negation wins", "docs/schemas/evidence/examples/log_v2.jsonl", false},
		{"the negation is scoped to its directory", "evidence.jsonl", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ignoredBy(patterns, tc.file); got != tc.want {
				t.Errorf("ignoredBy(%q) = %v, want %v", tc.file, got, tc.want)
			}
		})
	}
}

// ignorePatterns returns the ignore file's patterns in order, comments and
// blank lines dropped. Order is load-bearing: git applies the last pattern
// that matches.
func ignorePatterns(t *testing.T) []string {
	t.Helper()
	lines := strings.Split(read(t, ignoreFile), "\n")
	patterns := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}
	return patterns
}

// mainPackages returns the repository-relative directory of every main
// package in the tree, both modules included, sorted.
//
// It reads package clauses rather than asking `go list`, which would only
// see one module at a time and would put a toolchain invocation inside a
// test that has no other reason to need one.
func mainPackages(t *testing.T) []string {
	t.Helper()
	var dirs []string
	err := filepath.WalkDir(repoRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		raw, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if !declaresMain(string(raw)) {
			return nil
		}
		rel, rerr := filepath.Rel(repoRoot, filepath.Dir(p))
		if rerr != nil {
			return rerr
		}
		if dir := filepath.ToSlash(rel); !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the repository: %v", err)
	}
	slices.Sort(dirs)
	return dirs
}

// moduleRoot returns the repository-relative directory of the module a
// package belongs to: the nearest ancestor holding a go.mod, "." for the
// main module. That directory is where `go build ./<relative path>` runs
// from, and therefore where it drops the binary.
func moduleRoot(t *testing.T, dir string) string {
	t.Helper()
	for at := dir; ; at = path.Dir(at) {
		if _, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(at), "go.mod")); err == nil {
			return at
		}
		if at == "." {
			t.Fatalf("no go.mod above %s", dir)
		}
	}
}

// moduleLabel names a module root the way an error message should.
func moduleLabel(module string) string {
	if module == "." {
		return "the repository root"
	}
	return module
}

// declaresMain reports whether a Go source file is part of package main and
// part of the build. A file constrained to the "ignore" tag is neither: it
// is the conventional marker for a program `go build ./...` never compiles,
// so no build drops a binary for it.
func declaresMain(src string) bool {
	var isMain bool
	for _, line := range strings.Split(src, "\n") {
		switch {
		case strings.TrimSpace(line) == "//go:build ignore":
			return false
		case line == "package main":
			isMain = true
		}
	}
	return isMain
}

// ignoredBy reports whether the patterns ignore a repository-relative file
// path, applying the last one that matches as git does.
//
// It implements the subset of the gitignore syntax this repository's file
// uses: "!" negation, a "/" anywhere in the pattern anchoring it to the
// repository root, "*" matching within one path segment, and a trailing "/"
// restricting a pattern to directories. It does not implement "**", nor the
// rule that everything under an ignored directory is ignored — neither
// appears here, and both would make the matcher answer "ignored" more
// often, which is the direction a gate must not guess in.
func ignoredBy(patterns []string, file string) bool {
	ignored := false
	for _, pattern := range patterns {
		negated := strings.HasPrefix(pattern, "!")
		pattern = strings.TrimPrefix(pattern, "!")
		if strings.HasSuffix(pattern, "/") {
			continue
		}
		if matchIgnorePattern(pattern, file) {
			ignored = !negated
		}
	}
	return ignored
}

// matchIgnorePattern matches one pattern against one repository-relative
// path. An unanchored pattern — one with no separator — matches a file's
// name at any depth; anything else is matched segment by segment from the
// repository root.
func matchIgnorePattern(pattern, file string) bool {
	if !strings.Contains(pattern, "/") {
		ok, err := path.Match(pattern, path.Base(file))
		return err == nil && ok
	}
	want := strings.Split(strings.TrimPrefix(pattern, "/"), "/")
	got := strings.Split(file, "/")
	if len(want) != len(got) {
		return false
	}
	for i := range want {
		ok, err := path.Match(want[i], got[i])
		if err != nil || !ok {
			return false
		}
	}
	return true
}
