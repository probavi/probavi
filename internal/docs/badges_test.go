package docs_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// badgeRe matches a linked badge — [![alt](image)](target) — the only form
// the READMEs use. A badge that cannot be clicked asserts something without
// offering the run, the log or the file it read.
var badgeRe = regexp.MustCompile(`^\[!\[([^\]]+)\]\(([^)]+)\)\]\(([^)]+)\)$`)

// workflowBadgeRe matches the image URL of a GitHub workflow status badge,
// capturing the workflow file it reports on. The badge is served by name:
// rename the file and the badge does not break, it quietly reports nothing.
var workflowBadgeRe = regexp.MustCompile(`/actions/workflows/([^/]+)/badge\.svg`)

const (
	// staticBadgePrefix is shields.io's own endpoint: it renders the text
	// handed to it and reads no repository, so the slug rule cannot apply.
	staticBadgePrefix = "https://img.shields.io/badge/"
	// repoSlug is this repository. Anything that reads a repository must
	// read this one — a badge copied from elsewhere reports another
	// project's health under this project's name.
	repoSlug = "probavi/probavi"
)

// badge is one linked badge of a README.
type badge struct{ alt, image, target string }

// readmeBadges returns a document's linked badges in document order.
func readmeBadges(doc string) []badge {
	var out []badge
	for _, line := range strings.Split(doc, "\n") {
		if m := badgeRe.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			out = append(out, badge{alt: m[1], image: m[2], target: m[3]})
		}
	}
	return out
}

// TestBadgesPointAtThisRepository holds every badge to the two ways one
// goes wrong silently: it reads something that is not here any more, or it
// reads somebody else's project. Neither shows up as a broken image — a
// stale workflow badge renders "no status" and a foreign slug renders
// somebody else's green.
func TestBadgesPointAtThisRepository(t *testing.T) {
	found := readmeBadges(read(t, sourceDoc))
	if len(found) == 0 {
		t.Fatalf("%s carries no badges — this gate would pass vacuously", sourceDoc)
	}

	for _, b := range found {
		t.Run(b.alt, func(t *testing.T) {
			assertReadsThisRepository(t, b.image)
			assertReadsThisRepository(t, b.target)
			assertWorkflowStillExists(t, b)
		})
	}
}

// assertReadsThisRepository accepts a badge URL only if it can be traced
// back to something in this repository: a path that is here, a shields.io
// badge rendering text of its own, or a service reading this slug.
func assertReadsThisRepository(t *testing.T, url string) {
	t.Helper()
	if !strings.Contains(url, "://") {
		if _, err := os.Stat(filepath.Join(repoRoot, url)); err != nil {
			t.Errorf("links to %s, which is not in the repository", url)
		}
		return
	}
	if strings.HasPrefix(url, staticBadgePrefix) {
		return
	}
	if !strings.Contains(url, repoSlug) {
		t.Errorf("reads %s, which is not %s", url, repoSlug)
	}
}

// assertWorkflowStillExists checks a workflow status badge against the
// workflow files present, and against its own link: a badge showing one
// workflow while linking to another sends the reader off to check
// something else.
func assertWorkflowStillExists(t *testing.T, b badge) {
	t.Helper()
	m := workflowBadgeRe.FindStringSubmatch(b.image)
	if m == nil {
		return
	}
	workflow := filepath.Join(".github", "workflows", m[1])
	if _, err := os.Stat(filepath.Join(repoRoot, workflow)); err != nil {
		t.Errorf("reports on %s, which does not exist", workflow)
	}
	if !strings.HasSuffix(b.target, "/actions/workflows/"+m[1]) {
		t.Errorf("shows %s but links to %s", m[1], b.target)
	}
}

// TestTranslationsCarryTheSameBadges: the badge row states facts about the
// project, not about the document, and none of it is language-dependent —
// so a translation showing a different set, or none, is simply behind.
// Unlike the prose spans there is nothing to re-translate, which makes
// falling behind pure oversight.
func TestTranslationsCarryTheSameBadges(t *testing.T) {
	want := readmeBadges(read(t, sourceDoc))
	for _, file := range translationFiles(t) {
		t.Run(file, func(t *testing.T) {
			got := readmeBadges(read(t, file))
			if len(got) != len(want) {
				t.Fatalf("carries %d badges, %s carries %d", len(got), sourceDoc, len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("badge %d differs\n  %s: %+v\n  %s: %+v",
						i+1, sourceDoc, want[i], file, got[i])
				}
			}
		})
	}
}
