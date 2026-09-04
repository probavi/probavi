package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// validGameDayYAML pairs with two member drill files named alpha.yaml and
// beta.yaml (each validYAML); invalid cases are derived by replacement.
const validGameDayYAML = `name: gd-test
timeout: 1h
members:
  - name: alpha
    config: alpha.yaml
  - name: beta
    config: beta.yaml
    depends_on: [alpha]
`

// writeGameDay lays out a game-day file plus the two member drill configs
// it references and returns the game-day path.
func writeGameDay(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"alpha.yaml", "beta.yaml"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(validYAML), 0o600); err != nil {
			t.Fatalf("write member config: %v", err)
		}
	}
	path := filepath.Join(dir, "gameday.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write game-day config: %v", err)
	}
	return path
}

func TestLoadGameDay(t *testing.T) {
	path := writeGameDay(t, validGameDayYAML)
	g, err := LoadGameDay(path, nil)
	if err != nil {
		t.Fatalf("LoadGameDay: %v", err)
	}
	if g.Name != "gd-test" || g.Timeout.Std() != time.Hour || g.Path != path {
		t.Errorf("game-day = %+v, want gd-test/1h", g)
	}
	if !strings.HasPrefix(g.Hash, "sha256:") || len(g.Hash) != len("sha256:")+64 {
		t.Errorf("Hash = %q, want sha256:<64 hex>", g.Hash)
	}
	if g.Parallelism() != 1 {
		t.Errorf("Parallelism() = %d, want default 1", g.Parallelism())
	}
	base := filepath.Dir(path)
	for i, want := range []string{"alpha.yaml", "beta.yaml"} {
		if got := g.Members[i].Config; got != filepath.Join(base, want) {
			t.Errorf("members[%d].Config = %q, want resolved %q", i, got, filepath.Join(base, want))
		}
	}
	if len(g.Members[1].DependsOn) != 1 || g.Members[1].DependsOn[0] != "alpha" {
		t.Errorf("beta.DependsOn = %v, want [alpha]", g.Members[1].DependsOn)
	}
}

func TestParallelism(t *testing.T) {
	for _, tt := range []struct{ raw, want int }{{-1, 1}, {0, 1}, {1, 1}, {3, 3}} {
		if got := (&GameDay{MaxParallel: tt.raw}).Parallelism(); got != tt.want {
			t.Errorf("Parallelism(%d) = %d, want %d", tt.raw, got, tt.want)
		}
	}
}

func TestLoadGameDayRejects(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want []string
	}{
		{"unknown field", validGameDayYAML + "banana: yes\n", []string{"unknown field", "banana"}},
		{"missing name", strings.Replace(validGameDayYAML, "name: gd-test", `name: ""`, 1), []string{"name is required"}},
		{"missing timeout", strings.Replace(validGameDayYAML, "timeout: 1h\n", "", 1), []string{"timeout is required"}},
		{"negative max_parallel", strings.Replace(validGameDayYAML, "timeout: 1h", "timeout: 1h\nmax_parallel: -1", 1), []string{"max_parallel must not be negative"}},
		{"no members", "name: gd-test\ntimeout: 1h\nmembers: []\n", []string{"at least one member"}},
		{"member without name", strings.Replace(validGameDayYAML, "- name: alpha\n", "- name: \"\"\n", 1), []string{"members[0]: name is required"}},
		{"duplicate member name", strings.Replace(validGameDayYAML, "name: beta", "name: alpha", 1), []string{`members[1]: name "alpha" duplicates members[0]`}},
		{"member without config", strings.Replace(validGameDayYAML, "    config: alpha.yaml\n", "", 1), []string{"members[0]: config is required"}},
		{"self dependency", strings.Replace(validGameDayYAML, "depends_on: [alpha]", "depends_on: [beta]", 1), []string{"members[1]: depends_on must not reference the member itself"}},
		{"duplicate dependency", strings.Replace(validGameDayYAML, "depends_on: [alpha]", "depends_on: [alpha, alpha]", 1), []string{`members[1]: duplicate dependency "alpha"`}},
		{"unknown dependency", strings.Replace(validGameDayYAML, "depends_on: [alpha]", "depends_on: [gamma]", 1), []string{`members[1]: depends_on references unknown member "gamma"`}},
		{"dependency cycle", strings.Replace(validGameDayYAML, "- name: alpha\n", "- name: alpha\n    depends_on: [beta]\n", 1), []string{"dependency cycle involving members: alpha, beta"}},
		{"member config missing", strings.Replace(validGameDayYAML, "config: alpha.yaml", "config: nope.yaml", 1), []string{"members[0] (alpha)", "read config"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadGameDay(writeGameDay(t, tt.yaml), nil)
			if err == nil {
				t.Fatal("LoadGameDay accepted an invalid config")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

// TestLoadGameDaySharedLogRuleComparesFilesNotSpellings covers the way
// two members reach one log without writing it the same way. The guard
// exists to move that collision off the store's single-writer lock and
// into config load; keyed on the string as written, the two spellings
// below both start and meet at the lock instead.
func TestLoadGameDaySharedLogRuleComparesFilesNotSpellings(t *testing.T) {
	for name, spelling := range map[string]string{
		"a dot segment":       "/var/lib/probavi/./evidence.jsonl",
		"a doubled separator": "/var/lib/probavi//evidence.jsonl",
		"a parent step":       "/var/lib/probavi/logs/../evidence.jsonl",
		"a trailing dot":      "/var/lib/probavi/evidence.jsonl/.",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "alpha.yaml"), []byte(validYAML), 0o600); err != nil {
				t.Fatalf("write alpha: %v", err)
			}
			other := strings.Replace(validYAML, "/var/lib/probavi/evidence.jsonl", spelling, 1)
			if other == validYAML {
				t.Fatal("fixture did not change: the evidence path in validYAML moved")
			}
			if err := os.WriteFile(filepath.Join(dir, "beta.yaml"), []byte(other), 0o600); err != nil {
				t.Fatalf("write beta: %v", err)
			}
			gd := strings.Replace(validGameDayYAML, "timeout: 1h", "timeout: 1h\nmax_parallel: 2", 1)
			path := filepath.Join(dir, "gameday.yaml")
			if err := os.WriteFile(path, []byte(gd), 0o600); err != nil {
				t.Fatalf("write game-day: %v", err)
			}
			_, err := LoadGameDay(path, nil)
			if err == nil {
				t.Fatalf("LoadGameDay accepted %q and %q as different logs — they are one file",
					"/var/lib/probavi/evidence.jsonl", spelling)
			}
			// The operator's own spelling belongs in the message, so the
			// line they have to go and change is the line they wrote.
			if !strings.Contains(err.Error(), spelling) {
				t.Errorf("error %q does not carry the path as written, %q", err, spelling)
			}
		})
	}
}

func TestLoadGameDayRejectsInvalidMemberConfig(t *testing.T) {
	path := writeGameDay(t, validGameDayYAML)
	broken := strings.Replace(validYAML, "provider: docker", `provider: ""`, 1)
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "beta.yaml"), []byte(broken), 0o600); err != nil {
		t.Fatalf("write broken member: %v", err)
	}
	_, err := LoadGameDay(path, nil)
	if err == nil {
		t.Fatal("LoadGameDay accepted a game-day with an invalid member drill config")
	}
	for _, want := range []string{"members[1] (beta)", "sandbox.provider is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

// TestLoadGameDaySharedLogRule pins the docs/gameday.md §2 rule: members
// sharing an evidence log are fine sequentially and rejected with
// max_parallel above 1 (the store's single-writer lock would make them
// collide mid-exercise).
func TestLoadGameDaySharedLogRule(t *testing.T) {
	if _, err := LoadGameDay(writeGameDay(t, validGameDayYAML), nil); err != nil {
		t.Fatalf("sequential shared log must be accepted: %v", err)
	}
	parallel := strings.Replace(validGameDayYAML, "timeout: 1h", "timeout: 1h\nmax_parallel: 2", 1)
	_, err := LoadGameDay(writeGameDay(t, parallel), nil)
	if err == nil {
		t.Fatal("LoadGameDay accepted a shared evidence log with max_parallel 2")
	}
	for _, want := range []string{"alpha", "beta", "share evidence log", "max_parallel: 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestLoadGameDayFileProblems(t *testing.T) {
	if _, err := LoadGameDay(filepath.Join(t.TempDir(), "missing.yaml"), nil); err == nil {
		t.Error("LoadGameDay accepted a missing file")
	}
	if _, err := LoadGameDay(writeGameDay(t, ""), nil); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("empty config: got %v, want empty-config error", err)
	}
	if _, err := LoadGameDay(writeGameDay(t, "name: [broken\n"), nil); err == nil {
		t.Error("LoadGameDay accepted broken YAML syntax")
	}
	if _, err := LoadGameDay(writeGameDay(t, validGameDayYAML+"name: twice\n"), nil); err == nil {
		t.Error("LoadGameDay accepted a duplicate key")
	}
}

func TestLoadExampleGameDay(t *testing.T) {
	// The committed example must always load: this keeps README and
	// examples honest (AGENTS.md §5.5).
	g, err := LoadGameDay(filepath.Join("..", "..", "examples", "gameday.example.yaml"), nil)
	if err != nil {
		t.Fatalf("LoadGameDay(examples/gameday.example.yaml, nil): %v", err)
	}
	if g.Name != "shop-stack" || len(g.Members) != 2 {
		t.Errorf("example = %+v, want shop-stack with 2 members", g)
	}
	if deps := g.Members[1].DependsOn; len(deps) != 1 || deps[0] != g.Members[0].Name {
		t.Errorf("members[1].DependsOn = %v, want [%s]", deps, g.Members[0].Name)
	}
}
