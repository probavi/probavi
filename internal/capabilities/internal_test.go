package capabilities

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/probavi/probavi/internal/cli"
	"github.com/probavi/probavi/internal/notify"
	"github.com/probavi/probavi/internal/sandbox"
)

// internal_test.go unit-tests the guards Build relies on. They are what
// stops the manifest from publishing a dead documentation link or a
// maturity value no consumer knows how to read, and several of their
// branches are only reachable from here — the declared registries they
// check are compiled-in constants.

func TestValidStatus(t *testing.T) {
	for _, s := range Statuses() {
		if !validStatus(s) {
			t.Errorf("declared status %q is rejected", s)
		}
	}
	for _, s := range []string{"", "production", "Experimental", "ga"} {
		if validStatus(s) {
			t.Errorf("status %q was accepted", s)
		}
	}
}

func TestStatusesAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range Statuses() {
		if seen[s] {
			t.Errorf("duplicate status %q", s)
		}
		seen[s] = true
	}
	if !seen[StatusExperimental] || !seen[StatusBeta] || !seen[StatusStable] {
		t.Errorf("the vocabulary %v is missing a declared constant", Statuses())
	}
}

func TestRequireFile(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "docs", "schemas"), 0o755); err != nil {
		t.Fatalf("create fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "docs", "spec.md"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cases := []struct {
		name    string
		rel     string
		wantErr string
	}{
		{name: "no path declared", rel: ""},
		{name: "file that exists", rel: "docs/spec.md"},
		{name: "directory that exists", rel: "docs/schemas/"},
		{name: "file that does not exist", rel: "docs/absent.md", wantErr: "is not in the repository"},
		{name: "directory that does not exist", rel: "docs/absent/", wantErr: "is not in the repository"},
		{name: "directory where a file is expected", rel: "docs/schemas", wantErr: "is not a file"},
		{name: "file where a directory is expected", rel: "docs/spec.md/", wantErr: "is not a directory"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := requireFile(root, tc.rel, "fixture")
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("requireFile(%q) = %v, want nil", tc.rel, err)
			case tc.wantErr == "":
				return
			case err == nil:
				t.Fatalf("requireFile(%q) accepted a path it must reject", tc.rel)
			case !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("error %q does not contain %q", err, tc.wantErr)
			case !strings.Contains(err.Error(), "fixture"):
				t.Errorf("error %q does not name what was being checked", err)
			}
		})
	}
}

func TestKindWord(t *testing.T) {
	if got := kindWord(true); got != "directory" {
		t.Errorf("kindWord(true) = %q", got)
	}
	if got := kindWord(false); got != "file" {
		t.Errorf("kindWord(false) = %q", got)
	}
}

func TestNullable(t *testing.T) {
	if nullable("") != nil {
		t.Error("an empty string must render as null")
	}
	got := nullable("docs/i18n.md")
	if got == nil || *got != "docs/i18n.md" {
		t.Errorf("nullable(%q) = %v", "docs/i18n.md", got)
	}
}

// The section builders take their registry as a parameter precisely so
// these branches are reachable: the shipped registries are compiled-in
// constants and cannot be given a bad entry, but the validation that
// guards them still has to be proven to work.

func TestBuildProvidersRejectsBadEntries(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name    string
		entry   sandbox.Descriptor
		wantErr string
	}{
		{
			name:    "unknown maturity value",
			entry:   sandbox.Descriptor{ID: "demo", Status: "ga"},
			wantErr: "is not one of",
		},
		{
			name:    "docs path that does not exist",
			entry:   sandbox.Descriptor{ID: "demo", Status: StatusExperimental, Docs: "docs/absent.md"},
			wantErr: "is not in the repository",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildProviders(root, []sandbox.Descriptor{tc.entry})
			if err == nil {
				t.Fatal("buildProviders accepted an entry it must reject")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestBuildCommandsRejectsBadEntries(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name    string
		entry   cli.Command
		wantErr string
	}{
		{
			name:    "unknown maturity value",
			entry:   cli.Command{ID: "demo", Status: "ga"},
			wantErr: "is not one of",
		},
		{
			name:    "docs path that does not exist",
			entry:   cli.Command{ID: "demo", Status: StatusExperimental, Docs: "docs/absent.md"},
			wantErr: "is not in the repository",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildCommands(root, []cli.Command{tc.entry})
			if err == nil {
				t.Fatal("buildCommands accepted an entry it must reject")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestBuildNotificationsRejectsBadEntries(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name    string
		entry   notify.Transport
		wantErr string
	}{
		{
			name:    "unknown maturity value",
			entry:   notify.Transport{ID: "demo", Status: "ga"},
			wantErr: "is not one of",
		},
		{
			name:    "docs path that does not exist",
			entry:   notify.Transport{ID: "demo", Status: StatusExperimental, Docs: "docs/absent.md"},
			wantErr: "is not in the repository",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildNotifications(root, []notify.Transport{tc.entry})
			if err == nil {
				t.Fatal("buildNotifications accepted an entry it must reject")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestBuildLocalesRequiresItsSpec(t *testing.T) {
	if _, err := buildLocales(t.TempDir(), []string{"en"}); err == nil {
		t.Fatal("buildLocales accepted a repository without docs/i18n.md")
	}
}

// TestLoadProbeGoldenRejectsGarbage covers the parse path: a golden that
// is not JSON must fail here rather than yield an empty adapter entry.
// TestEngineNameMustNotRepeatTheEngineID covers both sides of the guard
// that thirteen manifests once tripped: the field reaching
// docs/capabilities.json as engine.name has to be a display name, and the
// one engine whose name really is its id has to keep it.
func TestEngineNameMustNotRepeatTheEngineID(t *testing.T) {
	probe := func(dir, engine string) *probeGolden {
		g := &probeGolden{}
		g.Payload.Name = dir
		g.Payload.Engine.Name = engine
		return g
	}
	manifest := func(dir, engineName string) *AdapterManifest {
		return &AdapterManifest{
			ID: dir, Name: "Demo", Status: StatusExperimental,
			EngineName: engineName, Since: Release{Stated: true, Value: "0.4.0"},
		}
	}

	if err := checkAdapterIdentity("demo", manifest("demo", "demo"), probe("demo", "demo")); err == nil {
		t.Error("accepted the probe's engine id as a display name")
	} else if !strings.Contains(err.Error(), "is the engine's id") {
		t.Errorf("err = %v, want it to say what is wrong with the value", err)
	}

	// Capitalisation is the ordinary case, not the bug: the probe declares
	// "postgresql" and the engine is called PostgreSQL.
	if err := checkAdapterIdentity("demo", manifest("demo", "Demo Engine"), probe("demo", "demo")); err != nil {
		t.Errorf("rejected a display name: %v", err)
	}

	// etcd spells its own name in lowercase, so the value that is wrong
	// everywhere else is right there.
	if !engineNamedLikeItsID["etcd"] {
		t.Fatal("etcd is no longer excepted — the guard below would reject its real name")
	}
	if err := checkAdapterIdentity("etcd", manifest("etcd", "etcd"), probe("etcd", "etcd")); err != nil {
		t.Errorf("rejected etcd's own name: %v", err)
	}
}

func TestLoadProbeGoldenRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "testdata"), 0o755); err != nil {
		t.Fatalf("create fixture: %v", err)
	}
	golden := filepath.Join(dir, "testdata", "probe_response.golden")
	if err := os.WriteFile(golden, []byte("not json\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := loadProbeGolden(dir); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("err = %v, want a parse failure", err)
	}
}

// TestAdapterDirsIgnoresStrayFiles proves discovery looks for adapters,
// not for everything under adapters/.
func TestAdapterDirsIgnoresStrayFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "adapters", "demo", "testdata"), 0o755); err != nil {
		t.Fatalf("create fixture: %v", err)
	}
	for path, content := range map[string]string{
		filepath.Join(root, "adapters", "README.md"):                                 "not an adapter",
		filepath.Join(root, "adapters", "demo", "testdata", "probe_response.golden"): "{}",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
	}
	// A directory with neither a golden nor a manifest is not an adapter.
	if err := os.MkdirAll(filepath.Join(root, "adapters", "scratch"), 0o755); err != nil {
		t.Fatalf("create fixture: %v", err)
	}
	dirs, err := AdapterDirs(root)
	if err != nil {
		t.Fatalf("AdapterDirs: %v", err)
	}
	if len(dirs) != 1 || filepath.Base(dirs[0]) != "demo" {
		t.Errorf("AdapterDirs = %v, want just demo", dirs)
	}
}
