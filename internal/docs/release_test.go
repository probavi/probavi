package docs_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// releaseWorkflow is the workflow that builds and publishes the release
// artifacts.
const releaseWorkflow = ".github/workflows/release.yml"

// adapterGlob is how that workflow enumerates the adapters it ships. The
// glob is the registry — a new adapter directory ships without anyone
// editing the workflow — which is only safe while the directories and the
// generated manifest agree. TestReleaseShipsExactlyTheDeclaredAdapters is
// what makes that true.
const adapterGlob = "for dir in adapters/*/; do"

// adapterDirs lists the directories under adapters/, which is exactly what
// the release workflow's glob expands to.
func adapterDirs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(repoRoot, "adapters"))
	if err != nil {
		t.Fatalf("list adapters: %v", err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Strings(dirs)
	return dirs
}

// TestReleaseNotesEscapeBackticksOnce keeps a shell escape from deleting
// the words it was meant to quote.
//
// The notes are assembled by a `run:` block, so a backtick inside a
// double-quoted echo is escaped as \` — one backslash. Written as \\`
// the shell reads a literal backslash followed by an *unescaped* backtick,
// which opens a command substitution and swallows everything up to the
// next one. v0.19.0's job log said ".deb\\: command not found", and the
// published v0.18.0 notes read "\\, \\ and \\ for amd64 and arm64"
// where the package extensions belong — the three words a reader of that
// section is there for.
func TestReleaseNotesEscapeBackticksOnce(t *testing.T) {
	for i, line := range strings.Split(read(t, releaseWorkflow), "\n") {
		if strings.Contains(line, "\\\\`") {
			t.Errorf("%s:%d escapes a backtick as \\\\` — the shell opens a command substitution "+
				"and eats the text: %s", releaseWorkflow, i+1, strings.TrimSpace(line))
		}
	}
}

// TestReleaseShipsExactlyTheDeclaredAdapters holds the set of binaries a
// release publishes to the set of adapters docs/capabilities.json declares.
//
// The release workflow builds every directory under adapters/. That is the
// right registry — adding an adapter should not require editing a workflow
// — but it means a directory that the manifest does not know about would
// be built, signed for, and published as a Probavi adapter. In a product
// whose output is evidence, shipping an undeclared binary is exactly the
// kind of drift AGENTS.md §5.8 exists to prevent: the manifest is the only
// permitted statement of what Probavi is, and a release must not exceed
// it.
//
// The generator reaches the opposite direction already: an adapter
// directory with a manifest but no probe golden fails capabilities
// generation. This closes the case it lets through — a directory with
// neither, silently skipped by the generator and silently shipped by the
// release.
func TestReleaseShipsExactlyTheDeclaredAdapters(t *testing.T) {
	declared := make(map[string]bool)
	for _, a := range readManifest(t).Adapters {
		declared[a.ID] = true
	}

	shipped := adapterDirs(t)
	if len(shipped) == 0 {
		t.Fatal("no adapter directories — this gate would pass vacuously")
	}

	for _, dir := range shipped {
		if !declared[dir] {
			t.Errorf("the release ships adapters/%s, which docs/capabilities.json does not declare — "+
				"give it a probe golden and an adapter.json, or move it out of adapters/", dir)
		}
		delete(declared, dir)
	}
	for id := range declared {
		t.Errorf("docs/capabilities.json declares adapter %q, but adapters/%s does not exist, "+
			"so no release artifact is built for it", id, id)
	}
}

// TestReleaseWorkflowEnumeratesAdaptersByGlob keeps the gate above
// load-bearing. It asserts nothing about the build flags; it only proves
// the workflow still derives its adapter list from the directories, so
// that holding those directories to the manifest holds the release to it
// too. Replacing the glob with a hand-written list is fine — but then this
// test must be replaced by one that reads that list.
func TestReleaseWorkflowEnumeratesAdaptersByGlob(t *testing.T) {
	if wf := read(t, releaseWorkflow); !strings.Contains(wf, adapterGlob) {
		t.Errorf("%s no longer contains %q, so TestReleaseShipsExactlyTheDeclaredAdapters "+
			"no longer proves anything about what the release publishes", releaseWorkflow, adapterGlob)
	}
}
