package docs_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const (
	repoRoot = "../.."
	// sourceDoc is the canonical English README. Every translation is
	// derived from the spans it marks, and from nothing else.
	sourceDoc = "README.md"
	// authorityNotice stays English in every translation on purpose: a
	// reader who cannot read the target language must still be able to
	// see which document binds.
	authorityNotice = "English is authoritative."
)

var (
	// markerRe matches a span delimiter in the English source:
	//   <!-- i18n:intro:start -->  …  <!-- i18n:intro:end -->
	markerRe = regexp.MustCompile(`(?m)^<!-- i18n:([a-z0-9-]+):(start|end) -->$`)
	// pinRe matches a span pin in a translation's header:
	//   <!-- i18n-span: intro sha256:<64 hex> -->
	pinRe = regexp.MustCompile(`(?m)^<!-- i18n-span: ([a-z0-9-]+) sha256:([0-9a-f]{64}) -->$`)
	// sourcePinRe matches the translation's statement of what it translates.
	sourcePinRe = regexp.MustCompile(`(?m)^<!-- i18n-source: (\S+) -->$`)
	// translationRe matches a translation filename, capturing its locale tag.
	translationRe = regexp.MustCompile(`^README\.([a-z]{2})\.md$`)
	// linkRe matches a translation link in the English README's language row.
	linkRe = regexp.MustCompile(`\(README\.([a-z]{2})\.md\)`)
	// versionRe matches a release-version claim (v1.4.0, v2.0, …).
	versionRe = regexp.MustCompile(`\bv[0-9]+\.[0-9]+`)
	// nativeReviewRe matches a translation's provenance pin:
	//   <!-- i18n-native-review: yes -->
	nativeReviewRe = regexp.MustCompile(`(?m)^<!-- i18n-native-review: (yes|no) -->$`)
)

// read returns a repository file as a string, failing the test if it is
// unreadable — a missing document is a broken gate, never a skip.
func read(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(raw)
}

// spanHash is the normative hash of a span (docs/i18n.md §7): SHA-256 over
// the whitespace-trimmed UTF-8 bytes between the marker lines. Trimming
// keeps a stray blank line from invalidating an otherwise current
// translation; every other byte counts.
func spanHash(content string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(content)))
	return hex.EncodeToString(sum[:])
}

// sourceSpans extracts the marked spans of the English README in document
// order, rejecting unbalanced, nested, or duplicated markers.
func sourceSpans(t *testing.T, doc string) map[string]string {
	t.Helper()
	spans := make(map[string]string)
	var openName string
	var openEnd int
	for _, m := range markerRe.FindAllStringSubmatchIndex(doc, -1) {
		name := doc[m[2]:m[3]]
		kind := doc[m[4]:m[5]]
		switch kind {
		case "start":
			if openName != "" {
				t.Fatalf("%s: span %q opens inside span %q — spans may not nest", sourceDoc, name, openName)
			}
			if _, dup := spans[name]; dup {
				t.Fatalf("%s: span %q is marked twice", sourceDoc, name)
			}
			openName, openEnd = name, m[1]
		case "end":
			if openName != name {
				t.Fatalf("%s: <!-- i18n:%s:end --> does not close an open span (open: %q)", sourceDoc, name, openName)
			}
			spans[name] = doc[openEnd:m[0]]
			openName = ""
		}
	}
	if openName != "" {
		t.Fatalf("%s: span %q is never closed", sourceDoc, openName)
	}
	if len(spans) == 0 {
		t.Fatalf("%s: no i18n spans found — the translation gate would pass vacuously", sourceDoc)
	}
	return spans
}

// translationFiles lists the README translations committed at the
// repository root.
func translationFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(repoRoot)
	if err != nil {
		t.Fatalf("read repository root: %v", err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && translationRe.MatchString(e.Name()) {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	return files
}

// pins parses the span pins recorded in a translation's header.
func pins(doc string) map[string]string {
	out := make(map[string]string)
	for _, m := range pinRe.FindAllStringSubmatch(doc, -1) {
		out[m[1]] = m[2]
	}
	return out
}

// verifyHeader holds a translation to the two things it must state about
// itself: what it translates, and that the English original binds.
func verifyHeader(t *testing.T, file, doc string) {
	t.Helper()
	src := sourcePinRe.FindStringSubmatch(doc)
	if src == nil {
		t.Fatalf("%s: missing <!-- i18n-source: %s --> header", file, sourceDoc)
	}
	if src[1] != sourceDoc {
		t.Errorf("%s: i18n-source is %q, want %q", file, src[1], sourceDoc)
	}
	if !strings.Contains(doc, authorityNotice) {
		t.Errorf("%s: the notice %q must appear verbatim, in English", file, authorityNotice)
	}
}

// verifyPins compares a translation's recorded pins with the current
// source spans in both directions: no span may go unpinned, and no pin may
// outlive the span it names.
func verifyPins(t *testing.T, file string, doc string, spans map[string]string) {
	t.Helper()
	recorded := pins(doc)
	for name, content := range spans {
		want := spanHash(content)
		got, ok := recorded[name]
		switch {
		case !ok:
			t.Errorf("%s: span %q is not pinned — add <!-- i18n-span: %s sha256:%s --> and translate it", file, name, name, want)
		case got != want:
			t.Errorf("%s: span %q is stale\n  pinned:  %s\n  current: %s\nRe-translate the span in %s, then update the pin.", file, name, got, want, sourceDoc)
		}
	}
	for name := range recorded {
		if _, ok := spans[name]; !ok {
			t.Errorf("%s: pins span %q, which %s no longer marks", file, name, sourceDoc)
		}
	}
}

// TestTranslationsAreCurrent is the gate: every translation pins every
// span of the English README, and every pin matches the current bytes. An
// edit to a marked span fails this test until each translation is
// refreshed and its pin updated.
func TestTranslationsAreCurrent(t *testing.T) {
	spans := sourceSpans(t, read(t, sourceDoc))
	files := translationFiles(t)
	if len(files) == 0 {
		t.Fatalf("no README.<tag>.md translations found — remove this gate or restore the files")
	}

	for _, file := range files {
		t.Run(file, func(t *testing.T) {
			doc := read(t, file)
			verifyHeader(t, file, doc)
			verifyPins(t, file, doc, spans)
		})
	}
}

// TestTranslationsClaimNoVersions keeps the translations out of the
// capability-claim business. Release versions, engine lists, and feature
// inventories live in the English Status section and, machine-readably, in
// docs/capabilities.json (AGENTS.md §5.8) — a translated copy of them is a
// claim nobody can keep in sync.
func TestTranslationsClaimNoVersions(t *testing.T) {
	for _, file := range translationFiles(t) {
		t.Run(file, func(t *testing.T) {
			for i, line := range strings.Split(read(t, file), "\n") {
				if m := versionRe.FindString(line); m != "" {
					t.Errorf("%s:%d: version claim %q — versions belong in the English README and docs/capabilities.json", file, i+1, m)
				}
			}
		})
	}
}

// TestLanguageRowListsEveryTranslation keeps the English README's language
// row complete: a translation nobody can navigate to is a translation
// nobody maintains.
func TestLanguageRowListsEveryTranslation(t *testing.T) {
	source := read(t, sourceDoc)
	linked := make(map[string]bool)
	for _, m := range linkRe.FindAllStringSubmatch(source, -1) {
		linked[m[1]] = true
	}
	for _, file := range translationFiles(t) {
		tag := translationRe.FindStringSubmatch(file)[1]
		if !linked[tag] {
			t.Errorf("%s is not linked from the language row of %s", file, sourceDoc)
		}
	}
	for tag := range linked {
		name := fmt.Sprintf("README.%s.md", tag)
		if _, err := os.Stat(filepath.Join(repoRoot, name)); err != nil {
			t.Errorf("%s links %s, which does not exist", sourceDoc, name)
		}
	}
}

// TestTranslationsStateTheirProvenance keeps a translation from implying a
// review it never had.
//
// Every one of these was made inside this project and passed the review
// docs/i18n.md §5 requires; none but Hungarian has been read by anyone who
// reads the language natively. A reader opening README.de.md learned none
// of that and had nowhere obvious to send a correction, while the
// specification had said it all along — which is the wrong place for it,
// because the person who can improve a German sentence is reading the
// German file and not the i18n spec.
//
// The pin is the machine-readable half of the sentence that now stands in
// each file's notice. It exists so the claim cannot quietly vanish from
// one translation during an edit: prose is what a reader is owed, a gate
// cannot read prose in four languages, but it can insist the claim is
// still declared.
//
// Deliberately not a draft label and not a gate on reviews arriving
// (docs/i18n.md §7.2): a label saying "do not trust this yet" discourages
// the reader most able to fix it, and a gate whose green depends on an
// outside volunteer appearing cannot be satisfied by doing better work,
// which is what every other gate here asks of whoever trips it.
func TestTranslationsStateTheirProvenance(t *testing.T) {
	for _, file := range translationFiles(t) {
		pins := nativeReviewRe.FindAllStringSubmatch(read(t, file), -1)
		switch len(pins) {
		case 1:
		case 0:
			t.Errorf("%s carries no <!-- i18n-native-review: yes|no --> pin, so its notice can "+
				"lose the provenance sentence without anything noticing (docs/i18n.md §7.3)", file)
		default:
			t.Errorf("%s carries %d i18n-native-review pins; exactly one is a statement, two are "+
				"a question", file, len(pins))
		}
	}
}
