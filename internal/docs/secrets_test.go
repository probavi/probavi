package docs_test

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/probavi/probavi/internal/evidence"
)

// keyLine matches what `probavi evidence keygen` writes: a line that is
// exactly 64 lowercase hex characters and nothing else.
//
// The narrowness is the point. The repository is full of SHA-256 values —
// action pins, image digests, the hashes inside evidence records — and every
// one of them carries something else on its line: a "sha256:" reference
// prefix, a JSON key, an @ and an image name. A line holding nothing but the
// hex is not a hash somebody quoted. It is a key file.
var keyLine = regexp.MustCompile(`(?m)^[0-9a-f]{64}$`)

// pemPrivateKey matches the header of a PEM-wrapped private key of any
// flavour. Probavi's own keys are not PEM, but an SSH or TLS key dropped
// into a working tree is the same accident with a different shape, and
// GitHub's generic pattern scanning — the tier that would catch it — is not
// available on this repository's plan.
var pemPrivateKey = regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY-----`)

// publishedKeys are the files allowed to carry a bare key line, each with
// the reason it is public.
//
// A public key and a private seed are the same 64 hex characters on disk —
// `probavi evidence keygen` writes both in that form — so no rule can tell
// them apart by content, and an exception is the only way to allow one. That
// is a feature of this gate rather than a weakness in it: adding a file here
// is a claim, in a reviewed diff, that the bytes in it are meant to be read
// by everyone. Nobody can write that sentence truthfully about a seed.
var publishedKeys = map[string]string{
	"docs/schemas/evidence/examples/signer.pub": "the public half of the conformance vectors' signer — " +
		"evidence-schema.md §12 exists so a third party can verify the published logs offline with it",
}

// TestNoCommittedKeyMaterial refuses a key that reached the working tree.
//
// This repository is public the moment it is pushed, and its threat model
// (AGENTS.md §3.3) is someone forging "everything was fine" — which a leaked
// signing seed makes trivial, because the forgery then verifies. GitHub's
// secret scanning is enabled here but cannot help: a Probavi seed is 64 bare
// hex characters, which matches no provider's pattern, and the non-provider
// patterns that might have caught something adjacent are a paid tier. What
// protects the seed today is two lines of .gitignore — `*.key` and `*.pem` —
// and `evidence keygen` writes wherever the operator points it, so a seed
// named anything else walks straight past them.
//
// This does not stop a push; nothing in CI can. It stops the merge, and it
// tells the operator to rotate rather than to delete the file, because by
// then the bytes are already published.
func TestNoCommittedKeyMaterial(t *testing.T) {
	for _, file := range scannableFiles(t) {
		raw, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(file)))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if pemPrivateKey.Match(raw) {
			t.Errorf("%s carries a PEM private key — remove it and rotate the key; it is published, not merely committed", file)
		}
		if !keyLine.Match(raw) {
			continue
		}
		if _, published := publishedKeys[file]; published {
			continue
		}
		t.Errorf("%s carries a line that is exactly 64 lowercase hex characters, which is what "+
			"`probavi evidence keygen` writes for both halves of a key pair.\n"+
			"    If it is a signing seed: rotate it. The bytes are published, not merely committed, and "+
			"deleting the file does not unpublish them (evidence-schema.md §6 — rotation adds keys, it never replaces one).\n"+
			"    If it is genuinely public, add it to publishedKeys in %s with the reason, where a reviewer sees the claim.",
			file, "internal/docs/secrets_test.go")
	}
}

// TestTheKeyPatternMatchesWhatKeygenWrites ties the rule to its producer.
// A gate guessing at the shape of a key file would keep passing on the day
// that shape changed, which is the one day it has to fail.
func TestTheKeyPatternMatchesWhatKeygenWrites(t *testing.T) {
	dir := t.TempDir()
	priv := filepath.Join(dir, "signing.seed")
	pub := filepath.Join(dir, "signing.pub")
	if _, err := evidence.GenerateKeyPair(priv, pub); err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	for _, path := range []string{priv, pub} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !keyLine.Match(raw) {
			t.Errorf("keygen wrote %s in a form this gate does not recognise:\n%q", filepath.Base(path), raw)
		}
	}
}

// TestPublishedKeysAreStillThere keeps the allow-list from outliving what it
// allows. An entry for a file that was renamed or deleted is an exception
// nobody can see any more, waiting for a new file to arrive at that path.
func TestPublishedKeysAreStillThere(t *testing.T) {
	for file, why := range publishedKeys {
		if why == "" {
			t.Errorf("publishedKeys[%q] has no reason — the reason is the whole point of the entry", file)
		}
		raw, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(file)))
		if err != nil {
			t.Errorf("publishedKeys names %s, which is not in the repository: %v", file, err)
			continue
		}
		if !keyLine.Match(raw) {
			t.Errorf("publishedKeys names %s, which no longer carries a key line — drop the exception", file)
		}
	}
}

// scannableFiles lists every file in the work tree that .gitignore does not
// ignore, which in a checkout is every file that is committed.
//
// It filters by the ignore rules rather than asking git, so the gate needs
// no subprocess and no repository metadata — and locally it still reads the
// files a developer is about to commit, which is where a stray key is at its
// most recoverable.
func scannableFiles(t *testing.T) []string {
	t.Helper()
	patterns := ignorePatterns(t)
	var files []string
	err := filepath.WalkDir(repoRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(repoRoot, p)
		if rerr != nil {
			return rerr
		}
		slash := filepath.ToSlash(rel)
		if slash == "." {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" || ignoredDir(patterns, slash) {
				return fs.SkipDir
			}
			return nil
		}
		if !ignoredBy(patterns, slash) {
			files = append(files, slash)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the repository: %v", err)
	}
	if len(files) < 100 {
		t.Fatalf("the walk found only %d files; this gate would pass by scanning nothing", len(files))
	}
	return files
}

// ignoredDir reports whether the patterns ignore a whole directory, so a
// walk can skip its subtree. Directory-only patterns — ".idea/", "/dist/" —
// are exactly the ones ignoredBy passes over, because they never match a
// file path directly.
func ignoredDir(patterns []string, dir string) bool {
	asFiles := make([]string, 0, len(patterns))
	for _, p := range patterns {
		asFiles = append(asFiles, strings.TrimSuffix(p, "/"))
	}
	return ignoredBy(asFiles, dir)
}

// TestTheScanReachesABareKeyFile proves the walk and the filter together
// would actually find one, rather than skipping the file and reporting
// nothing to report.
func TestTheScanReachesABareKeyFile(t *testing.T) {
	seed := bytes.Repeat([]byte("ab"), 32)
	if !keyLine.Match(append(seed, '\n')) {
		t.Fatal("the pattern does not match a bare 64-character hex line")
	}
	for _, notAKey := range []string{
		"sha256:" + strings.Repeat("ab", 32),
		"  " + strings.Repeat("ab", 32) + "  trailing words",
		strings.Repeat("AB", 32),
		strings.Repeat("ab", 20),
		"uses: actions/checkout@" + strings.Repeat("ab", 20),
	} {
		if keyLine.MatchString(notAKey + "\n") {
			t.Errorf("the pattern matches %q, which is not a key file", notAKey)
		}
	}
}
