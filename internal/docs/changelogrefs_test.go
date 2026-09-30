package docs_test

import (
	"regexp"
	"strings"
	"testing"
)

// releaseHeading matches one released section's heading; linkRef matches the
// reference definition at the foot of the file that turns it into a link.
var (
	releaseHeading = regexp.MustCompile(`(?m)^## \[(\d+\.\d+\.\d+)\] - \d{4}-\d{2}-\d{2}$`)
	linkRef        = regexp.MustCompile(`(?m)^\[(\d+\.\d+\.\d+)\]: \S+$`)
	unreleasedRef  = regexp.MustCompile(`(?m)^\[Unreleased\]: \S+/compare/v(\d+\.\d+\.\d+)\.\.\.HEAD$`)
)

// TestEveryReleaseHeadingHasALinkReference keeps the Keep-a-Changelog
// bracket syntax from rendering as literal brackets.
//
// This gate exists because the thing it checks had already gone wrong
// three times: 0.33.0, 0.34.0 and 0.35.0 each shipped without their
// reference, and `[Unreleased]` still compared from v0.32.0 four releases
// later. Nothing noticed, because a missing reference does not break a
// build — it only makes a heading a reader cannot click, in the file that
// is the release record.
func TestEveryReleaseHeadingHasALinkReference(t *testing.T) {
	changelog := read(t, "CHANGELOG.md")

	headings := releaseHeading.FindAllStringSubmatch(changelog, -1)
	if len(headings) == 0 {
		t.Fatal("CHANGELOG.md declares no released version — this gate would pass vacuously")
	}
	refs := make(map[string]bool)
	for _, m := range linkRef.FindAllStringSubmatch(changelog, -1) {
		refs[m[1]] = true
	}
	if len(refs) == 0 {
		t.Fatal("CHANGELOG.md carries no link references at all — this gate is watching a block that moved")
	}
	for _, m := range headings {
		if !refs[m[1]] {
			t.Errorf("CHANGELOG.md has a section for %s but no [%s]: reference, so the heading "+
				"renders as literal brackets", m[1], m[1])
		}
	}
}

// TestUnreleasedComparesFromTheNewestRelease pins the other half. The
// Unreleased link is the diff since the last tag, so a stale base makes it
// claim that work already released is still pending.
func TestUnreleasedComparesFromTheNewestRelease(t *testing.T) {
	changelog := read(t, "CHANGELOG.md")

	m := unreleasedRef.FindStringSubmatch(changelog)
	if m == nil {
		t.Fatal("CHANGELOG.md has no [Unreleased]: …/compare/vX.Y.Z...HEAD reference — " +
			"this gate is watching a line that moved")
	}
	newest := releaseHeading.FindStringSubmatch(changelog)
	if newest == nil {
		t.Fatal("CHANGELOG.md declares no released version — this gate would pass vacuously")
	}
	// The newest release is the first heading in the file: entries are
	// prepended, which is what Keep a Changelog's reverse-chronological
	// order means.
	if m[1] != newest[1] {
		t.Errorf("[Unreleased] compares from v%s, but the newest release is %s — the link "+
			"presents released work as pending", m[1], newest[1])
	}
}

// TestLinkReferencesHaveNoOrphans is the gate in the other direction: a
// reference for a version no section declares is either a typo or a
// release that was renamed, and both leave a dangling link.
func TestLinkReferencesHaveNoOrphans(t *testing.T) {
	changelog := read(t, "CHANGELOG.md")

	sections := make(map[string]bool)
	for _, m := range releaseHeading.FindAllStringSubmatch(changelog, -1) {
		sections[m[1]] = true
	}
	refs := linkRef.FindAllStringSubmatch(changelog, -1)
	if len(sections) == 0 || len(refs) == 0 {
		t.Fatal("CHANGELOG.md has no sections or no references — this gate would pass vacuously")
	}
	var orphans []string
	for _, m := range refs {
		if !sections[m[1]] {
			orphans = append(orphans, m[1])
		}
	}
	if len(orphans) > 0 {
		t.Errorf("CHANGELOG.md defines [%s]: with no matching section", strings.Join(orphans, "], ["))
	}
	if t.Failed() {
		t.Logf("%d sections, %d references", len(sections), len(refs))
	}
}
