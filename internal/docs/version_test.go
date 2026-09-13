package docs_test

import (
	"regexp"
	"strconv"
	"testing"
)

// goDirective matches the language version the main module declares. The
// patch component is optional: go.mod may carry either form.
var goDirective = regexp.MustCompile(`(?m)^go (\d+\.\d+)(?:\.\d+)?$`)

// goRequirement matches the toolchain version the quickstart asks a reader
// to have before the first command.
var goRequirement = regexp.MustCompile(`You need Go (\d+\.\d+)\+`)

// TestQuickstartNamesTheGoVersionTheModuleRequires keeps the first sentence
// of the quickstart true.
//
// It said "Go 1.24+" from the initial public release until go.mod moved to
// 1.25 and nobody moved the sentence with it. Nothing caught it: the
// version badge at the top of the same file reads go.mod live, so the
// README disagreed with itself in two places a reader sees within one
// screen of each other, and the reader with the older toolchain is the one
// who finds out — either by downloading a toolchain they did not ask for,
// or, with GOTOOLCHAIN=local, by a build that stops before the quickstart's
// first step.
func TestQuickstartNamesTheGoVersionTheModuleRequires(t *testing.T) {
	module := goDirective.FindStringSubmatch(read(t, "go.mod"))
	if module == nil {
		t.Fatal("go.mod declares no go directive — this gate would pass vacuously")
	}
	stated := goRequirement.FindStringSubmatch(read(t, sourceDoc))
	if stated == nil {
		t.Fatalf("%s no longer says which Go version it needs; %s expects the form \"You need Go X.Y+\"",
			sourceDoc, "TestQuickstartNamesTheGoVersionTheModuleRequires")
	}
	if stated[1] != module[1] {
		t.Errorf("%s asks the reader for Go %s+, but go.mod requires %s — a build following the quickstart "+
			"verbatim needs the newer one", sourceDoc, stated[1], module[1])
	}
}

// changelogRelease matches the newest released heading of CHANGELOG.md.
// [Unreleased] carries no date and is skipped by the date requirement, so
// the first match is the version this repository currently claims to be.
var changelogRelease = regexp.MustCompile(`(?m)^## \[(\d+\.\d+\.\d+)\] - \d{4}-\d{2}-\d{2}$`)

// currentVersion is the newest version CHANGELOG.md declares released.
func currentVersion(t *testing.T) string {
	t.Helper()
	m := changelogRelease.FindStringSubmatch(read(t, "CHANGELOG.md"))
	if m == nil {
		t.Fatal("CHANGELOG.md declares no released version — this gate would pass vacuously")
	}
	return m[1]
}

// versionClaim is one place that must name the current release, and the
// pattern that finds it. Every capture group must equal that version.
//
// The list is deliberately narrow. A blanket search for version-shaped
// tokens would flag the things that must NOT move: the adapters' own
// adapterVersion numbers (postgres is at 0.3.0 quite independently), the
// contract versions probavi-adapter/0 and probavi-evidence/2, and the
// historical notes about v0.1.0's module path. Those are not claims about
// this release, and rewriting them would turn accurate history into a
// lie. What is listed here is only what goes stale.
type versionClaim struct {
	file    string
	what    string
	pattern *regexp.Regexp
}

var versionClaims = []versionClaim{
	{sourceDoc, "the Status line's release claim",
		regexp.MustCompile(`Released as \*\*v(\d+\.\d+\.\d+)\*\*`)},
	{sourceDoc, "the download example's tag",
		regexp.MustCompile(`\$ tag=v(\d+\.\d+\.\d+) `)},
	{sourceDoc, "the packaged install example",
		regexp.MustCompile(`probavi_(\d+\.\d+\.\d+)_amd64\.deb`)},
	{"docs/docker.md", "the image tag",
		regexp.MustCompile(`ghcr\.io/probavi/probavi:(\d+\.\d+\.\d+)`)},
	{"docs/docker.md", "the reproduce-the-image build argument",
		regexp.MustCompile(`--build-arg VERSION=(\d+\.\d+\.\d+)`)},
	{"docs/packaging.md", "the release-download URLs",
		regexp.MustCompile(`releases/download/v(\d+\.\d+\.\d+)/`)},
	{"docs/packaging.md", "the install examples' version variable",
		regexp.MustCompile(`\$ ver=(\d+\.\d+\.\d+) `)},
	{"docs/packaging.md", "the package filenames",
		regexp.MustCompile(`probavi[-_](?:adapter-\w+[-_])?(\d+\.\d+\.\d+)[-_.]`)},
}

// TestDocumentedVersionsMatchTheChangelog keeps every install instruction
// on the version this repository says it is.
//
// A stale version in an install command is a special kind of wrong: it
// looks tested, it is copied verbatim, and it fails on the reader's
// machine with a 404 rather than in anyone's CI. Bumping a release means
// touching a dozen files across three documents, and the one that gets
// missed is always the one somebody runs.
func TestDocumentedVersionsMatchTheChangelog(t *testing.T) {
	want := currentVersion(t)
	for _, claim := range versionClaims {
		t.Run(claim.file+": "+claim.what, func(t *testing.T) {
			matches := claim.pattern.FindAllStringSubmatch(read(t, claim.file), -1)
			if len(matches) == 0 {
				t.Fatalf("%s no longer contains %s — this gate is watching a line that moved",
					claim.file, claim.what)
			}
			for _, m := range matches {
				if m[1] != want {
					t.Errorf("%s names version %s in %s, but CHANGELOG.md released %s",
						claim.file, m[1], claim.what, want)
				}
			}
		})
	}
}

// devVersion matches the version the binary stamps into itself.
var devVersion = regexp.MustCompile(`(?m)^var version = "(\d+\.\d+\.\d+)-dev"$`)

// TestBinaryVersionTracksTheChangelog holds `probavi version` to the
// release it belongs to.
//
// The stamp is what a drill records as env.probavi_version and what an
// auditor reads back out of a signed record, so a build claiming to be
// the previous release is not a cosmetic slip — it is a signed record
// naming the wrong software. It has drifted before: 0.2.0 shipped and the
// stamp stayed at 0.2.0-dev through everything that followed.
func TestBinaryVersionTracksTheChangelog(t *testing.T) {
	const source = "cmd/probavi/run.go"
	m := devVersion.FindStringSubmatch(read(t, source))
	if m == nil {
		t.Fatalf("%s no longer declares a `var version = \"X.Y.Z-dev\"` stamp", source)
	}
	if want := currentVersion(t); m[1] != want {
		t.Errorf("%s stamps %s-dev, but CHANGELOG.md released %s — a signed record would "+
			"name the wrong build", source, m[1], want)
	}
}

// verifierTag matches a published tag of the independent verifier module.
var verifierTag = regexp.MustCompile(`spec/evidence/v(\d+)\.(\d+)\.(\d+)`)

// verifierPin matches the version the documentation tells people to pin.
var verifierPin = regexp.MustCompile(`probavi-evidence-verify@v(\d+\.\d+\.\d+)`)

// newestVerifierVersion is the highest spec/evidence tag CHANGELOG.md
// names. Highest, not first: the entry announcing v0.2.0 sits above the
// one announcing v0.3.0, because both belong to the same release and the
// older one was written first.
func newestVerifierVersion(t *testing.T) string {
	t.Helper()
	var best [3]int
	var found string
	for _, m := range verifierTag.FindAllStringSubmatch(read(t, "CHANGELOG.md"), -1) {
		var v [3]int
		for i := range v {
			n, err := strconv.Atoi(m[i+1])
			if err != nil {
				t.Fatalf("unparseable verifier tag %q: %v", m[0], err)
			}
			v[i] = n
		}
		if v[0] > best[0] ||
			(v[0] == best[0] && v[1] > best[1]) ||
			(v[0] == best[0] && v[1] == best[1] && v[2] > best[2]) {
			best, found = v, m[1]+"."+m[2]+"."+m[3]
		}
	}
	if found == "" {
		t.Fatal("CHANGELOG.md names no spec/evidence tag — this gate would pass vacuously")
	}
	return found
}

// TestVerifierPinMatchesItsOwnNewestTag holds the documented `go install`
// of the independent verifier to the newest version of *that module*.
//
// It deliberately does not track the release version. spec/evidence is a
// separate Go module with its own tags precisely so it can move when the
// verifier changes and stay still when it does not — 0.3.1 fixes a
// container image and leaves the verifier untouched, so its pin must
// remain v0.3.0. Tying the two together would have forced a meaningless
// module tag on every release, and a wrong pin is worse than a stale one:
// the module proxy records a version permanently, so an install command
// naming a tag that was never cut cannot be repaired afterwards.
func TestVerifierPinMatchesItsOwnNewestTag(t *testing.T) {
	want := newestVerifierVersion(t)
	for _, file := range []string{sourceDoc, "spec/evidence/README.md"} {
		t.Run(file, func(t *testing.T) {
			matches := verifierPin.FindAllStringSubmatch(read(t, file), -1)
			if len(matches) == 0 {
				t.Fatalf("%s no longer shows how to install the independent verifier", file)
			}
			for _, m := range matches {
				if m[1] != want {
					t.Errorf("%s pins the verifier at v%s, but the newest tag CHANGELOG.md "+
						"names is v%s", file, m[1], want)
				}
			}
		})
	}
}
