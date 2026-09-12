package docs_test

import (
	"os"
	"path"
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

// selfVerifying names the two assets no checksum line may cover.
// SHA256SUMS cannot list itself, and the sigstore bundle carries its own
// proof — a checksum for it would restate, less strongly, what verifying
// the bundle already establishes.
var selfVerifying = map[string]bool{
	"SHA256SUMS":              true,
	"provenance.intoto.jsonl": true,
}

// uploadArgs returns the dist/ paths the workflow hands `gh release
// create` — the definitive list of what a release publishes.
func uploadArgs(t *testing.T) []string {
	t.Helper()
	wf := read(t, releaseWorkflow)
	start := strings.Index(wf, "gh release create ")
	if start < 0 {
		t.Fatalf("%s no longer calls `gh release create`, so this gate proves nothing", releaseWorkflow)
	}
	end := strings.Index(wf[start:], "--draft")
	if end < 0 {
		t.Fatalf("%s: no --draft after `gh release create`; the upload list cannot be read", releaseWorkflow)
	}
	var args []string
	for _, f := range strings.Fields(wf[start : start+end]) {
		if strings.HasPrefix(f, "dist/") {
			args = append(args, f)
		}
	}
	if len(args) == 0 {
		t.Fatalf("%s: `gh release create` names no dist/ artifact", releaseWorkflow)
	}
	return args
}

// checksummed returns the basename patterns every `sha256sum --` call in
// the workflow covers. The globs are read where they are written rather
// than restated here, so a glob that stops matching a published class
// fails this test instead of shipping an unchecksummed asset.
func checksummed(t *testing.T) map[string]bool {
	t.Helper()
	covered := make(map[string]bool)
	for _, line := range strings.Split(read(t, releaseWorkflow), "\n") {
		_, rest, found := strings.Cut(line, "sha256sum -- ")
		if !found {
			continue
		}
		// Stop at the redirection or the pipe that consumes the output.
		rest, _, _ = strings.Cut(rest, ">")
		rest, _, _ = strings.Cut(rest, "|")
		rest = strings.TrimSuffix(strings.TrimSpace(rest), ")")
		for _, pattern := range strings.Fields(rest) {
			covered[pattern] = true
		}
	}
	return covered
}

// TestEveryPublishedAssetIsChecksummed holds the SHA256SUMS file to the
// upload list.
//
// v0.28.0 published 323 assets and checksummed 292: the 29 Homebrew
// formulae and the provenance bundle were outside every glob. The bundle
// belongs outside one; the formulae did not, and the reason they were
// missing is ordering rather than intent — a formula pins checksums read
// from SHA256SUMS, so none exists when the file is first written, and the
// second pass that covers them is easy to forget when a new asset class
// arrives. Whatever a future release adds to the upload list, this test
// fails until a glob reaches it.
func TestEveryPublishedAssetIsChecksummed(t *testing.T) {
	covered := checksummed(t)
	if len(covered) == 0 {
		t.Fatal("no `sha256sum --` call in the release workflow — nothing published is checksummed")
	}

	for _, arg := range uploadArgs(t) {
		name := path.Base(arg)
		if selfVerifying[name] {
			continue
		}
		if !covered[name] {
			t.Errorf("the release uploads %s, which no `sha256sum --` glob in %s covers (%q) — "+
				"a downloader has nothing to check it against",
				arg, releaseWorkflow, strings.Join(sortedKeys(covered), " "))
		}
	}
}

// sortedKeys renders a set for an error message, in a stable order.
func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
