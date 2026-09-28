package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mutation_test.go holds the cases a mutation run found missing: each one
// is a change to the code that every other test accepted. They are here
// together because that is what they have in common — a gap in what the
// suite asserts rather than a gap in what it covers.

// withCheck replaces the sample drill's single check with another one.
func withCheck(check string) string {
	return strings.Replace(validYAML, "- builtin: service_healthy", check, 1)
}

// mustLoad loads a drill config that has to be accepted.
func mustLoad(t *testing.T, yaml string) *Config {
	t.Helper()
	cfg, err := Load(writeConfig(t, yaml), nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

// mustReject loads a drill config that has to be refused, and returns the
// error so the caller can say what it must name.
func mustReject(t *testing.T, yaml string) error {
	t.Helper()
	_, err := Load(writeConfig(t, yaml), nil)
	if err == nil {
		t.Fatal("Load accepted a configuration it must refuse")
	}
	return err
}

// TestRowCountBoundsAreAcceptedAtTheirEdges: the validator refuses a
// negative bound and a minimum above its maximum, and both rules have an
// edge that is an ordinary configuration rather than a corner case. Zero is
// what a table allowed to be empty says, and a minimum equal to its maximum
// is the most precise count a drill can assert. Only the refusals were
// tested, so a validator one step stricter would have refused the two
// configurations a user is most likely to write.
func TestRowCountBoundsAreAcceptedAtTheirEdges(t *testing.T) {
	for name, bounds := range map[string]string{
		"a minimum of zero": "    min: 0",
		"a maximum of zero": "    max: 0",
		"both zero":         "    min: 0\n    max: 0",
		"an exact count":    "    min: 100\n    max: 100",
	} {
		t.Run(name, func(t *testing.T) {
			mustLoad(t, withCheck("- builtin: row_count\n    table: orders\n"+bounds))
		})
	}
}

// TestBuiltinRefusesAKindTheRegistryDoesNotCallBuiltin: the registry holds
// one kind that is not a builtin — `sql`, configured with sql and expect
// rather than with the builtin key — and the guard has two halves because
// of it. Absent from the registry is the half every test covered. Present
// but not a builtin is the half none did, and it is the one that would let
// `builtin: sql` reach internal/checks, which has no such kind to run.
func TestBuiltinRefusesAKindTheRegistryDoesNotCallBuiltin(t *testing.T) {
	err := mustReject(t, withCheck("- builtin: sql"))
	for _, want := range []string{"unknown builtin", "sql"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to carry %q", err, want)
		}
	}
}

// TestADurationIsPositiveOrItIsRefused: the guard reads `<= 0`, and both of
// its edges were unasserted. Zero is the one a config actually produces —
// `timeout: 0s` parses cleanly and means a drill that may never run — while
// one nanosecond is where a guard reaching one step further would go
// unnoticed, because every realistic duration is far above it.
func TestADurationIsPositiveOrItIsRefused(t *testing.T) {
	if err := mustReject(t, strings.Replace(validYAML, "timeout: 30m", "timeout: 0s", 1)); !strings.Contains(err.Error(), "must be positive") {
		t.Errorf("error = %v, want it to say a duration must be positive", err)
	}
	cfg := mustLoad(t, strings.Replace(validYAML, "timeout: 30m", "timeout: 1ns", 1))
	if got := cfg.Sandbox.Timeout.Std(); got != 1 {
		t.Errorf("timeout = %v, want the one nanosecond the config asked for", got)
	}
}

// TestAWebhookURLIsAddressableOverHTTP: the shape check is three conditions
// and only one of them had a case. Each refuses a different way of writing
// an address the notifier cannot post to, and each is reachable from a
// configuration somebody would write — a scheme it does not speak, or a
// scheme it does speak with nothing to send to. The drill would otherwise
// learn this at the moment it had something to report, which is the moment
// a notification exists for.
func TestAWebhookURLIsAddressableOverHTTP(t *testing.T) {
	for name, url := range map[string]string{
		// The scheme condition.
		"a scheme that is not http":  "ftp://example.test/hook",
		"a scheme with no transport": "mailto:ops@example.test",
		"no scheme at all":           "example.test/hook",
		// The host condition, which only a URL the scheme condition
		// accepts can reach.
		"http with no host":  "http:///hook",
		"https with no host": "https://",
	} {
		t.Run(name, func(t *testing.T) {
			yaml := validYAML + "notify:\n  webhooks:\n    - url: \"" + url + "\"\n"
			if err := mustReject(t, yaml); !strings.Contains(err.Error(), "notify.webhooks[0]") {
				t.Errorf("error = %v, want it to name the webhook", err)
			}
		})
	}
	mustLoad(t, validYAML+"notify:\n  webhooks:\n    - url: https://example.test/hook\n")
}

// writeGameDayFiles lays out a game-day beside one drill config per member
// and returns the game-day path. It takes the member configs rather than
// writing the same one for each, because two of the cases below turn on
// what a member's own drill says.
func writeGameDayFiles(t *testing.T, content string, members map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, yaml := range members {
		if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(yaml), 0o600); err != nil {
			t.Fatalf("write member %s: %v", name, err)
		}
	}
	path := filepath.Join(dir, "gameday.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write game-day: %v", err)
	}
	return path
}

// sameMembers is writeGameDayFiles' common case: every named member runs
// the sample drill.
func sameMembers(names ...string) map[string]string {
	m := make(map[string]string, len(names))
	for _, n := range names {
		m[n] = validYAML
	}
	return m
}

// TestAChainOfThreeIsNotACycle: the walk queues a member when its last
// dependency clears, and the existing cases never reach a member whose
// dependency itself had one. A walk that queued on the wrong count would
// report a dependency cycle for a plain chain — database, then application,
// then cache — which is the ordinary shape of a game-day rather than an
// exotic one.
func TestAChainOfThreeIsNotACycle(t *testing.T) {
	yaml := `name: gd-chain
timeout: 1h
members:
  - name: alpha
    config: alpha.yaml
  - name: beta
    config: beta.yaml
    depends_on: [alpha]
  - name: gamma
    config: gamma.yaml
    depends_on: [beta]
`
	if _, err := LoadGameDay(writeGameDayFiles(t, yaml, sameMembers("alpha", "beta", "gamma")), nil); err != nil {
		t.Fatalf("LoadGameDay: %v — a chain of three is an ordering, not a cycle", err)
	}
}

// TestTheCycleDiagnosticNamesOnlyTheStuckMembers: the message exists to say
// which members to go and edit, so naming members that are perfectly
// ordered costs it exactly what it is for. Asserted in both directions,
// because a condition that lists everyone still contains the right answer.
func TestTheCycleDiagnosticNamesOnlyTheStuckMembers(t *testing.T) {
	yaml := `name: gd-partial
timeout: 1h
members:
  - name: first
    config: first.yaml
  - name: beta
    config: beta.yaml
    depends_on: [gamma]
  - name: gamma
    config: gamma.yaml
    depends_on: [beta]
  - name: last
    config: last.yaml
`
	_, err := LoadGameDay(writeGameDayFiles(t, yaml, sameMembers("first", "beta", "gamma", "last")), nil)
	if err == nil {
		t.Fatal("LoadGameDay accepted a game-day with a cycle in it")
	}
	for _, want := range []string{"beta", "gamma"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to name the stuck member %q", err, want)
		}
	}
	for _, unwanted := range []string{"first", "last"} {
		if strings.Contains(err.Error(), unwanted) {
			t.Errorf("error = %v, want it not to name %q, which is not in the cycle", err, unwanted)
		}
	}
}

// TestTwoMembersNamingOneLogBySpellingsThatResolveTogether: the guard moves
// a collision from the store's single-writer lock, mid-exercise, to config
// load — so it has to compare files rather than spellings. Every case before
// this used identical spellings, where comparing either way agrees; a
// relative path beside the absolute one it resolves to is the case the
// resolution exists for, and the one a person writes when two drill configs
// come from different places.
func TestTwoMembersNamingOneLogBySpellingsThatResolveTogether(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	const rel = "shared-evidence.jsonl"
	withLog := func(path string) string {
		return strings.Replace(validYAML, "path: /var/lib/probavi/evidence.jsonl", "path: "+path, 1)
	}
	members := map[string]string{
		"alpha": withLog(rel),
		"beta":  withLog(filepath.Join(cwd, rel)),
	}
	yaml := `name: gd-logs
timeout: 1h
max_parallel: 2
members:
  - name: alpha
    config: alpha.yaml
  - name: beta
    config: beta.yaml
`
	_, err = LoadGameDay(writeGameDayFiles(t, yaml, members), nil)
	if err == nil {
		t.Fatal("LoadGameDay accepted two members whose evidence paths resolve to one file")
	}
	if !strings.Contains(err.Error(), "share evidence log") {
		t.Errorf("error = %v, want the shared-evidence-log diagnostic", err)
	}
}
