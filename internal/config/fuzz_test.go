package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fuzzFile gives a fuzz target one path to rewrite, rather than a new
// temporary directory per iteration: the loaders take a path, and
// creating a directory millions of times would measure the filesystem
// instead of the parser.
func fuzzFile(f *testing.F, name string) (dir, path string) {
	f.Helper()
	dir = f.TempDir()
	return dir, filepath.Join(dir, name)
}

// FuzzLoad drives the drill-config loader over arbitrary bytes.
//
// It is the first thing a drill runs and it reads a file the operator
// wrote, through a YAML decoder in strict mode and then through
// validation that reports every problem in one pass. Both halves are
// parsers of untrusted shape by this repository's own threat model.
//
// Beyond survival, the property downstream depends on: a config that
// comes back has been validated, so the fields the core dereferences
// without checking are populated. A loader that returned both a config
// and an error, or neither, would leave that ambiguous.
func FuzzLoad(f *testing.F) {
	f.Add([]byte(validYAML))
	f.Add([]byte("drill:\n  name: x\n"))
	f.Add([]byte("{}"))
	f.Add([]byte("[]"))
	f.Add([]byte("a: &a [*a]\n"))
	f.Add([]byte("sandbox:\n  provider: docker\n  timeout: notaduration\n"))
	f.Add([]byte(strings.Repeat("a:\n b:\n", 64)))
	f.Add([]byte(""))
	f.Add([]byte("\x00\xff"))

	_, path := fuzzFile(f, "drill.yaml")
	f.Fuzz(func(t *testing.T, data []byte) {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Skipf("write fixture: %v", err)
		}
		cfg, err := Load(path, nil)
		switch {
		case err != nil && cfg != nil:
			t.Fatalf("both a config and an error: %+v / %v", cfg, err)
		case err == nil && cfg == nil:
			t.Fatal("neither a config nor an error")
		case err != nil:
			if err.Error() == "" {
				t.Fatal("rejection with no message")
			}
			return
		}
		// Accepted: everything validation promises the core.
		if cfg.Sandbox.Provider == "" {
			t.Fatal("accepted a config with no sandbox provider")
		}
		if cfg.Sandbox.Timeout == 0 {
			t.Fatal("accepted a config with no sandbox timeout")
		}
		if len(cfg.Checks) == 0 {
			t.Fatal("accepted a config with no checks — the drill would prove nothing")
		}
		if cfg.Evidence.Path == "" {
			t.Fatal("accepted a config with no evidence path")
		}
		// The hash reaches a signed record, so it is never absent.
		if !strings.HasPrefix(cfg.Hash, "sha256:") {
			t.Fatalf("config hash = %q, want a sha256: digest", cfg.Hash)
		}
	})
}

// FuzzLoadGameDay drives the game-day loader, including the member walk.
//
// Two valid member files sit beside the fuzzed one so that an input which
// happens to name them reaches loadMembers — the dependency graph, the
// shared-evidence-log guard and the per-member Load — rather than
// stopping at a file that does not exist.
func FuzzLoadGameDay(f *testing.F) {
	f.Add([]byte(validGameDayYAML))
	f.Add([]byte("name: g\ntimeout: 1h\nmembers: []\n"))
	f.Add([]byte("name: g\ntimeout: 1h\nmax_parallel: 2\nmembers:\n  - name: a\n    config: alpha.yaml\n  - name: b\n    config: beta.yaml\n"))
	f.Add([]byte("name: g\ntimeout: 1h\nmembers:\n  - name: a\n    config: alpha.yaml\n    depends_on: [a]\n"))
	f.Add([]byte("members:\n  - name: a\n    config: ../../../etc/passwd\n"))
	f.Add([]byte(""))
	f.Add([]byte("\x00"))

	dir, path := fuzzFile(f, "gameday.yaml")
	for _, member := range []string{"alpha.yaml", "beta.yaml"} {
		if err := os.WriteFile(filepath.Join(dir, member), []byte(validYAML), 0o600); err != nil {
			f.Fatalf("write member: %v", err)
		}
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Skipf("write fixture: %v", err)
		}
		g, err := LoadGameDay(path, nil)
		switch {
		case err != nil && g != nil:
			t.Fatalf("both a game-day and an error: %+v / %v", g, err)
		case err == nil && g == nil:
			t.Fatal("neither a game-day nor an error")
		case err != nil:
			if err.Error() == "" {
				t.Fatal("rejection with no message")
			}
			return
		}
		if g.Name == "" {
			t.Fatal("accepted a game-day with no name")
		}
		if len(g.Members) == 0 {
			t.Fatal("accepted a game-day with no members")
		}
		if g.Parallelism() < 1 {
			t.Fatalf("parallelism = %d — the runner would start nothing", g.Parallelism())
		}
		if !strings.HasPrefix(g.Hash, "sha256:") {
			t.Fatalf("game-day hash = %q, want a sha256: digest", g.Hash)
		}
	})
}
