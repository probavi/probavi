package docs_test

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// engines_test.go holds the README's engine table, and the release each
// adapter claims to have shipped in, to sources that cannot be typed from
// memory: the generated manifest and CHANGELOG.md.
//
// docs/capabilities.json states `since` per adapter, and nothing in the
// generator can tell whether the release named there is the right one — it
// is a fact about this repository's history, not about its code. That is
// what the gate below reads out of the changelog.

const (
	engineTableStart = "<!-- capabilities:engines:start -->"
	engineTableEnd   = "<!-- capabilities:engines:end -->"
	changelog        = "CHANGELOG.md"
	unreleasedLabel  = "Unreleased"
)

// changelogHeading matches a section heading of CHANGELOG.md. A released
// section carries a date; `## [Unreleased]` does not, which is exactly the
// distinction this gate turns on.
var changelogHeading = regexp.MustCompile(`(?m)^## \[([^\]]+)\](?: - (\d{4}-\d{2}-\d{2}))?$`)

// section is one changelog entry, in file order — newest first.
type section struct {
	version  string
	released bool
	body     string
}

// changelogSections splits CHANGELOG.md into its entries.
func changelogSections(t *testing.T) []section {
	t.Helper()
	doc := read(t, changelog)
	locs := changelogHeading.FindAllStringSubmatchIndex(doc, -1)
	if len(locs) == 0 {
		t.Fatalf("%s has no section headings — this gate would pass vacuously", changelog)
	}
	sections := make([]section, 0, len(locs))
	for i, loc := range locs {
		end := len(doc)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		sections = append(sections, section{
			version:  doc[loc[2]:loc[3]],
			released: loc[4] >= 0,
			body:     doc[loc[1]:end],
		})
	}
	return sections
}

// names reports whether a changelog section names an adapter — by its id
// or by its display name, because an entry announcing an adapter writes
// the display name and the entries after it write the path.
func names(s section, id, display string) bool {
	return strings.Contains(s.body, id) || strings.Contains(s.body, display)
}

// TestAdapterSinceMatchesTheChangelog holds `since` to the release history.
//
// The field is the one thing in an adapter's manifest that no test could
// otherwise contradict: a wrong release is a plausible-looking string, and
// it is published on the README and in the manifest downstream consumers
// read. Three questions decide it, and each has a wrong answer worth
// catching — a release that never happened, a release that does not
// mention the adapter, and a release later than the one the adapter first
// appeared in.
func TestAdapterSinceMatchesTheChangelog(t *testing.T) {
	m := readManifest(t)
	sections := changelogSections(t)

	for _, a := range m.Adapters {
		t.Run(a.ID, func(t *testing.T) {
			path := fmt.Sprintf("adapters/%s", a.ID)
			if a.Since == nil {
				assertUnreleased(t, sections, a.ID, a.Name, path)
				return
			}
			assertReleasedIn(t, sections, *a.Since, a.ID, a.Name, path)
		})
	}
}

// assertUnreleased checks an adapter that states no release: it must be in
// the tree and in the Unreleased section, and in no released one. The
// release that moves that heading therefore fails this gate until `since`
// is filled in, which is what makes the null a statement rather than an
// omission nobody notices.
func assertUnreleased(t *testing.T, sections []section, id, display, path string) {
	t.Helper()
	announced := false
	for _, s := range sections {
		switch {
		case !s.released && s.version == unreleasedLabel:
			announced = announced || names(s, id, display)
		case s.released && strings.Contains(s.body, path):
			t.Errorf("%s states since: null, but %s %s already ships it — set since to that release",
				id, changelog, s.version)
		}
	}
	if !announced {
		t.Errorf("%s states since: null, but %s does not announce it under [%s]",
			id, changelog, unreleasedLabel)
	}
}

// assertReleasedIn checks an adapter that names a release.
func assertReleasedIn(t *testing.T, sections []section, since, id, display, path string) {
	t.Helper()
	found := -1
	for i, s := range sections {
		if s.released && s.version == since {
			found = i
			break
		}
	}
	if found < 0 {
		t.Fatalf("%s states since: %s, which is not a released section of %s", id, since, changelog)
	}
	if !names(sections[found], id, display) {
		t.Errorf("%s states since: %s, but that section of %s never mentions the adapter",
			id, since, changelog)
	}
	// Sections are newest first, so everything after the claimed one is
	// older. An adapter cannot have shipped before the release it says it
	// first shipped in.
	for _, older := range sections[found+1:] {
		if older.released && strings.Contains(older.body, path) {
			t.Errorf("%s states since: %s, but %s already names %s in %s",
				id, since, changelog, path, older.version)
		}
	}
}

// engineTable returns the generated block of the README, without its
// markers.
func engineTable(t *testing.T) string {
	t.Helper()
	doc := read(t, sourceDoc)
	start := strings.Index(doc, engineTableStart)
	end := strings.Index(doc, engineTableEnd)
	if start < 0 || end < start {
		t.Fatalf("%s carries no engine table block", sourceDoc)
	}
	return doc[start+len(engineTableStart) : end]
}

// TestEngineTableHasOneRowPerAdapter proves the block is the manifest's
// list and not a hand-kept copy of it. `go generate ./...` writes it and
// CI fails on a dirty tree, so this gate is the second half: it fails on a
// README whose table was edited without the manifest moving.
func TestEngineTableHasOneRowPerAdapter(t *testing.T) {
	m := readManifest(t)
	var rows []string
	for _, line := range strings.Split(engineTable(t), "\n") {
		if strings.HasPrefix(line, "| ") && !strings.HasPrefix(line, "| --- ") &&
			!strings.HasPrefix(line, "| Engine ") {
			rows = append(rows, line)
		}
	}
	if len(rows) != len(m.Adapters) {
		t.Fatalf("engine table has %d rows, manifest has %d adapters", len(rows), len(m.Adapters))
	}
	for i, a := range m.Adapters {
		if !strings.Contains(rows[i], a.Name) {
			t.Errorf("row %d is %q, want the manifest's adapter %s in that position", i, rows[i], a.Name)
		}
		want := "unreleased"
		if a.Since != nil {
			want = *a.Since
		}
		if !strings.Contains(rows[i], want) {
			t.Errorf("%s row does not carry its release %q: %s", a.ID, want, rows[i])
		}
	}
}

// TestEngineTableIsOutsideTheTranslatedSpans keeps a generated block from
// ever landing inside a translated span.
//
// A translation pins the sha256 of each span it renders (docs/i18n.md §7).
// A generated table inside one would invalidate every translation on any
// day an adapter changed — and a translated table would be a capability
// claim in a language no gate reads. The spans and the block have to stay
// disjoint, and that is a property worth a test rather than a look.
func TestEngineTableIsOutsideTheTranslatedSpans(t *testing.T) {
	doc := read(t, sourceDoc)
	start := strings.Index(doc, engineTableStart)
	end := strings.Index(doc, engineTableEnd)
	if start < 0 || end < start {
		t.Fatalf("%s carries no engine table block", sourceDoc)
	}
	markers := markerRe.FindAllStringSubmatchIndex(doc, -1)
	open := map[string]int{}
	for _, loc := range markers {
		name, kind := doc[loc[2]:loc[3]], doc[loc[4]:loc[5]]
		if kind == "start" {
			open[name] = loc[0]
			continue
		}
		from, ok := open[name]
		if !ok {
			continue // translations_test.go reports unbalanced spans.
		}
		delete(open, name)
		if start < loc[1] && end > from {
			t.Errorf("the engine table overlaps the %q translated span (%d-%d)", name, from, loc[1])
		}
	}
}
