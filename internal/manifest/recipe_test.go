package manifest_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// specPath is the normative document. The recipe under test is read out of
// it rather than copied here: a copy would let the published instructions
// and the rule drift apart silently, which is the one failure §3.1 exists
// to prevent.
const specPath = "../../docs/backup-manifest.md"

// TestThePublishedRecipeAgreesWithTheRule is the gate §3.1 promises.
//
// The backup manifest is written by whoever takes the backup, on a host
// that has never heard of Probavi, and no helper ships to do it
// (backup-manifest.md §9.1). What makes that deferral safe is that the
// documented way to produce the digest produces *this* digest — because a
// tree hash framed differently does not disagree occasionally, it
// disagrees on every drill forever, and each disagreement signs a fail
// naming a backup that is fine.
func TestThePublishedRecipeAgreesWithTheRule(t *testing.T) {
	recipe := recipeFromSpec(t)
	requireTools(t, "bash", "find", "sort", "stat", "sha256sum", "readlink")

	dir := t.TempDir()
	tree := filepath.Join(dir, "backup")
	// The awkward cases on purpose: nesting, a name that sorts before a
	// sibling directory's, a symlink, and an empty file.
	writeFile(t, filepath.Join(tree, "a.txt"), "one")
	writeFile(t, filepath.Join(tree, "a", "b"), "two")
	writeFile(t, filepath.Join(tree, "sub", "deep", "c.bin"), "\x00\x01\x02")
	writeFile(t, filepath.Join(tree, "empty"), "")
	if err := os.Symlink("../a/b", filepath.Join(tree, "sub", "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	script := filepath.Join(dir, "recipe.sh")
	if err := os.WriteFile(script, []byte(recipe), 0o600); err != nil {
		t.Fatalf("write recipe: %v", err)
	}
	cmd := exec.Command("bash", script)
	cmd.Env = append(os.Environ(), "dir="+tree)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the published recipe did not run: %v\n%s", err, out)
	}
	fromShell := "sha256:" + strings.Fields(string(out))[0]

	fromRule := digestOf(t, tree)
	if fromShell != fromRule {
		t.Errorf("the recipe in %s §3.1 and the rule in §3 disagree:\n  shell: %s\n  rule:  %s",
			specPath, fromShell, fromRule)
	}
}

// recipeFromSpec extracts the shell block of §3.1, comment lines included,
// so the test runs what a reader would copy.
func recipeFromSpec(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read %s: %v", specPath, err)
	}
	_, after, ok := strings.Cut(string(raw), "### 3.1")
	if !ok {
		t.Fatalf("%s has no §3.1; the section the recipe lives in was renamed", specPath)
	}
	_, after, ok = strings.Cut(after, "```sh\n")
	if !ok {
		t.Fatalf("%s §3.1 carries no shell block", specPath)
	}
	recipe, _, ok := strings.Cut(after, "```")
	if !ok {
		t.Fatalf("%s §3.1 has an unterminated shell block", specPath)
	}
	if !strings.Contains(recipe, "sha256sum") {
		t.Fatalf("the block found in §3.1 is not the digest recipe:\n%s", recipe)
	}
	return recipe
}

func requireTools(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s is not on PATH: the recipe names its dependencies, and this host lacks one", name)
		}
	}
}
