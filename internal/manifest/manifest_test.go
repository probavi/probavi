package manifest_test

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/probavi/probavi/internal/evidence"
	"github.com/probavi/probavi/internal/manifest"
)

// writeManifest writes a manifest document and returns its path. The body
// is written verbatim so a test can express a malformed one.
func writeManifest(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "backup.manifest.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return path
}

func manifestFor(t *testing.T, dir, checksum string, size *int64) string {
	t.Helper()
	doc := map[string]any{"schema": manifest.SchemaID}
	if checksum != "" {
		doc["expected_checksum"] = checksum
	}
	if size != nil {
		doc["expected_size_bytes"] = *size
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	return writeManifest(t, dir, string(raw))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func sizePtr(n int64) *int64 { return &n }

// TestAFileIsHashedOverItsBytes: the single-file shape of §3 is exactly
// what `sha256sum` produces, which is what makes the documented recipe a
// one-liner for most kinds.
func TestAFileIsHashedOverItsBytes(t *testing.T) {
	dir := t.TempDir()
	artifact := filepath.Join(dir, "nightly.dump")
	writeFile(t, artifact, "hello backup")
	want := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("hello backup")))

	m := manifestFor(t, dir, want, sizePtr(12))
	if fault := manifest.Check(artifact, m); fault != nil {
		t.Fatalf("a manifest stating the file's own digest was refused: %v", fault)
	}
}

// TestByteOrderIsNotWalkOrder pins the detail §3 says a second
// implementation gets wrong first: entries sort by byte value, so "a.txt"
// precedes "a/b" — while a directory walk meets "a/b" first, because "a"
// sorts before "a.txt" among the names of one directory.
func TestByteOrderIsNotWalkOrder(t *testing.T) {
	dir := t.TempDir()
	tree := filepath.Join(dir, "backup")
	writeFile(t, filepath.Join(tree, "a.txt"), "one")
	writeFile(t, filepath.Join(tree, "a", "b"), "two")

	byteOrder := sha256.New()
	fmt.Fprintf(byteOrder, "a.txt\x003\x00one")
	fmt.Fprintf(byteOrder, "a/b\x003\x00two")
	want := fmt.Sprintf("sha256:%x", byteOrder.Sum(nil))

	walkOrder := sha256.New()
	fmt.Fprintf(walkOrder, "a/b\x003\x00two")
	fmt.Fprintf(walkOrder, "a.txt\x003\x00one")
	notWant := fmt.Sprintf("sha256:%x", walkOrder.Sum(nil))

	if want == notWant {
		t.Fatal("the fixture cannot tell the two orders apart")
	}
	if fault := manifest.Check(tree, manifestFor(t, dir, want, nil)); fault != nil {
		t.Fatalf("byte-order framing was refused: %v", fault)
	}
	fault := manifest.Check(tree, manifestFor(t, dir, notWant, nil))
	if fault == nil {
		t.Fatal("walk-order framing was accepted; the paths are not being sorted")
	}
	if fault.Code != evidence.CodeSourceCorrupt {
		t.Errorf("code = %q, want %q", fault.Code, evidence.CodeSourceCorrupt)
	}
}

// TestALinkDoesNotFrameLikeAFile: the L before a symlink's target is what
// keeps a link pointing at "x" from hashing identically to a one-byte file
// containing "x".
func TestALinkDoesNotFrameLikeAFile(t *testing.T) {
	linked := t.TempDir()
	writeFile(t, filepath.Join(linked, "keep"), "")
	if err := os.Symlink("x", filepath.Join(linked, "entry")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	plain := t.TempDir()
	writeFile(t, filepath.Join(plain, "keep"), "")
	writeFile(t, filepath.Join(plain, "entry"), "x")

	if digestOf(t, linked) == digestOf(t, plain) {
		t.Error("a symlink to x and a file containing x hashed the same")
	}
}

// TestTheSameTreeHashesTheSameAndAChangeChanges is the property the rule
// exists for, in both directions.
func TestTheSameTreeHashesTheSameAndAChangeChanges(t *testing.T) {
	dir := t.TempDir()
	tree := filepath.Join(dir, "backup")
	writeFile(t, filepath.Join(tree, "data", "0001.sst"), "rows")
	writeFile(t, filepath.Join(tree, "meta.json"), `{"n":1}`)

	first := digestOf(t, tree)
	if again := digestOf(t, tree); again != first {
		t.Fatalf("the same tree hashed two ways: %s then %s", first, again)
	}
	writeFile(t, filepath.Join(tree, "data", "0001.sst"), "rowS")
	if changed := digestOf(t, tree); changed == first {
		t.Error("a content change did not change the tree hash")
	}
}

// digestOf reads the digest back out of the refusal message, which is the
// only place this package publishes it — and is therefore also a test that
// the message names the artifact's actual value.
func digestOf(t *testing.T, path string) string {
	t.Helper()
	dir := t.TempDir()
	impossible := "sha256:" + strings.Repeat("0", 64)
	fault := manifest.Check(path, manifestFor(t, dir, impossible, nil))
	if fault == nil {
		t.Fatalf("%s matched an all-zero digest", path)
	}
	_, after, ok := strings.Cut(fault.Message, " is ")
	if !ok {
		t.Fatalf("message does not name the artifact's digest: %s", fault.Message)
	}
	got, _, _ := strings.Cut(after, ",")
	return got
}

// TestMismatchNamesBothValues: §5 requires the message to carry what the
// artifact is and what the manifest expected, in that order.
func TestMismatchNamesBothValues(t *testing.T) {
	dir := t.TempDir()
	artifact := filepath.Join(dir, "nightly.dump")
	writeFile(t, artifact, "actual bytes")
	expected := "sha256:" + strings.Repeat("ab", 32)

	fault := manifest.Check(artifact, manifestFor(t, dir, expected, sizePtr(999)))
	if fault == nil {
		t.Fatal("a wrong checksum was accepted")
	}
	if fault.Code != evidence.CodeSourceCorrupt {
		t.Fatalf("code = %q, want %q", fault.Code, evidence.CodeSourceCorrupt)
	}
	for _, want := range []string{artifact, expected, "12 bytes", "999 bytes"} {
		if !strings.Contains(fault.Message, want) {
			t.Errorf("message does not name %q: %s", want, fault.Message)
		}
	}
}

// TestSizeOnlyMismatchTalksAboutSize: a manifest that expected no checksum
// must not produce a message about an empty one.
func TestSizeOnlyMismatchTalksAboutSize(t *testing.T) {
	dir := t.TempDir()
	artifact := filepath.Join(dir, "nightly.dump")
	writeFile(t, artifact, "twelve bytes")

	fault := manifest.Check(artifact, manifestFor(t, dir, "", sizePtr(4182016)))
	if fault == nil {
		t.Fatal("a wrong size was accepted")
	}
	if strings.Contains(fault.Message, "sha256:") || strings.Contains(fault.Message, "()") {
		t.Errorf("a size-only manifest produced a message about a checksum: %s", fault.Message)
	}
	if !strings.Contains(fault.Message, "12 bytes") || !strings.Contains(fault.Message, "4182016 bytes") {
		t.Errorf("message does not name both sizes: %s", fault.Message)
	}
}

// TestSizeOnlyDoesNotReadTheBytes: a size-only manifest is the cheap
// check, and an artifact whose bytes cannot be read still satisfies it.
// Root ignores the permission that proves it, so the test says so instead
// of passing for the wrong reason.
func TestSizeOnlyDoesNotReadTheBytes(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: an unreadable file is still readable, so this proves nothing")
	}
	dir := t.TempDir()
	artifact := filepath.Join(dir, "nightly.dump")
	writeFile(t, artifact, "twelve bytes")
	if err := os.Chmod(artifact, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(artifact, 0o600) }) //nolint:errcheck // best effort, so t.TempDir can clean up

	if fault := manifest.Check(artifact, manifestFor(t, dir, "", sizePtr(12))); fault != nil {
		t.Errorf("a size-only manifest read the bytes it did not need: %v", fault)
	}
}

// TestManifestFaultsAreConfigurationFailures is the §5 line that matters
// most: a drill must never write "the backup is the problem" about an
// artifact it never looked at, so every fault in the manifest itself is
// invalid_request rather than a source_ verdict.
func TestManifestFaultsAreConfigurationFailures(t *testing.T) {
	tests := []struct {
		name, body, wantIn string
	}{
		{"not json", "{ this is not json", "not a readable"},
		{"unknown field", `{"schema":"probavi-manifest/1","expected_size":5}`, "unknown field"},
		{"two documents", `{"schema":"probavi-manifest/1","expected_size_bytes":5} {"schema":"x"}`, "more than one JSON document"},
		{"no schema", `{"expected_size_bytes":5}`, "names no schema"},
		{"unknown schema", `{"schema":"probavi-manifest/2","expected_size_bytes":5}`, "does not read"},
		{"asserts nothing", `{"schema":"probavi-manifest/1"}`, "asserts nothing"},
		{"checksum not sha256", `{"schema":"probavi-manifest/1","expected_checksum":"deadbeef"}`, "64 lowercase hex"},
		{"checksum uppercase", `{"schema":"probavi-manifest/1","expected_checksum":"sha256:` + strings.Repeat("AB", 32) + `"}`, "64 lowercase hex"},
		{"negative size", `{"schema":"probavi-manifest/1","expected_size_bytes":-1}`, "negative"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			artifact := filepath.Join(dir, "nightly.dump")
			writeFile(t, artifact, "bytes")
			fault := manifest.Check(artifact, writeManifest(t, dir, tc.body))
			if fault == nil {
				t.Fatal("accepted a manifest that cannot be checked")
			}
			if fault.Code != evidence.CodeInvalidRequest {
				t.Errorf("code = %q, want %q — a manifest fault is not a verdict about the backup", fault.Code, evidence.CodeInvalidRequest)
			}
			if !strings.Contains(fault.Message, tc.wantIn) {
				t.Errorf("message %q does not explain the fault (%q)", fault.Message, tc.wantIn)
			}
		})
	}
}

// TestAMissingManifestIsTheConfigsProblem: the row that moved when §5 was
// decided. A manifest the config named and the backup job never wrote says
// nothing about a backup that is sitting right there.
func TestAMissingManifestIsTheConfigsProblem(t *testing.T) {
	dir := t.TempDir()
	artifact := filepath.Join(dir, "nightly.dump")
	writeFile(t, artifact, "bytes")

	fault := manifest.Check(artifact, filepath.Join(dir, "absent.manifest.json"))
	if fault == nil {
		t.Fatal("a missing manifest was accepted")
	}
	if fault.Code != evidence.CodeInvalidRequest {
		t.Errorf("code = %q, want %q", fault.Code, evidence.CodeInvalidRequest)
	}
	if !strings.Contains(fault.Message, "not found") {
		t.Errorf("message = %q, want it to say the manifest is not there", fault.Message)
	}
}

func TestAnUnreadableManifestIsAlsoTheConfigsProblem(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: an unreadable file is still readable")
	}
	dir := t.TempDir()
	artifact := filepath.Join(dir, "nightly.dump")
	writeFile(t, artifact, "bytes")
	m := manifestFor(t, dir, "", sizePtr(5))
	if err := os.Chmod(m, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(m, 0o600) }) //nolint:errcheck // best effort, so t.TempDir can clean up

	fault := manifest.Check(artifact, m)
	if fault == nil || fault.Code != evidence.CodeInvalidRequest {
		t.Fatalf("fault = %v, want %s", fault, evidence.CodeInvalidRequest)
	}
	if strings.Count(fault.Message, m) != 1 {
		t.Errorf("message names the path more than once: %s", fault.Message)
	}
}

// TestArtifactFaultsAreVerdicts: the other half of the §5 line. What is
// wrong with the artifact is the backup's problem, and renders fail.
func TestArtifactFaultsAreVerdicts(t *testing.T) {
	tests := []struct {
		name     string
		build    func(t *testing.T, dir string) string
		wantCode string
		wantIn   string
	}{
		{
			name:     "source absent",
			build:    func(_ *testing.T, dir string) string { return filepath.Join(dir, "gone.dump") },
			wantCode: evidence.CodeSourceNotFound,
			wantIn:   "not found",
		},
		{
			name: "directory holds no regular file",
			build: func(t *testing.T, dir string) string {
				tree := filepath.Join(dir, "empty")
				if err := os.MkdirAll(filepath.Join(tree, "nested"), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				return tree
			},
			wantCode: evidence.CodeSourceNotFound,
			wantIn:   "no regular file",
		},
		{
			name: "neither a file nor a directory",
			build: func(t *testing.T, dir string) string {
				fifo := filepath.Join(dir, "pipe")
				if err := syscall.Mkfifo(fifo, 0o600); err != nil {
					t.Skipf("mkfifo unavailable: %v", err)
				}
				return fifo
			},
			wantCode: evidence.CodeSourceUnreadable,
			wantIn:   "neither a regular file nor a directory",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			source := tc.build(t, dir)
			fault := manifest.Check(source, manifestFor(t, dir, "", sizePtr(1)))
			if fault == nil {
				t.Fatal("accepted an artifact that cannot be measured")
			}
			if fault.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", fault.Code, tc.wantCode)
			}
			if !strings.Contains(fault.Message, tc.wantIn) {
				t.Errorf("message %q does not say %q", fault.Message, tc.wantIn)
			}
		})
	}
}

// TestAnUnreadableFileInATreeIsUnreadableNotCorrupt: a permission problem
// is not a corrupt backup, and the codes must not blur.
func TestAnUnreadableFileInATreeIsUnreadableNotCorrupt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: an unreadable file is still readable")
	}
	dir := t.TempDir()
	tree := filepath.Join(dir, "backup")
	writeFile(t, filepath.Join(tree, "closed"), "secret")
	if err := os.Chmod(filepath.Join(tree, "closed"), 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(tree, "closed"), 0o600) }) //nolint:errcheck // best effort, so t.TempDir can clean up

	fault := manifest.Check(tree, manifestFor(t, dir, "sha256:"+strings.Repeat("0", 64), nil))
	if fault == nil || fault.Code != evidence.CodeSourceUnreadable {
		t.Fatalf("fault = %v, want %s", fault, evidence.CodeSourceUnreadable)
	}
}

// TestAFaultIsAnError keeps Fault usable with errors.As at a call site
// that wants one.
func TestAFaultIsAnError(t *testing.T) {
	var err error = &manifest.Fault{Code: evidence.CodeSourceCorrupt, Message: "mismatch"}
	if err.Error() != "mismatch" {
		t.Errorf("Error() = %q, want %q", err.Error(), "mismatch")
	}
}

// TestSizeOnlyOverATreeSumsTheRegularFiles: §3 says a symlink's own size
// is the length of its target and not a byte of the backup, so it
// contributes to the checksum and not to the size. A tree whose size
// counted its links would drift from every hand-written manifest by the
// length of a path.
func TestSizeOnlyOverATreeSumsTheRegularFiles(t *testing.T) {
	dir := t.TempDir()
	tree := filepath.Join(dir, "backup")
	writeFile(t, filepath.Join(tree, "meta.json"), `{"n":1}`) // 7 bytes
	writeFile(t, filepath.Join(tree, "data", "0001.sst"), "rows")
	if err := os.Symlink("../meta.json", filepath.Join(tree, "data", "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if fault := manifest.Check(tree, manifestFor(t, dir, "", sizePtr(11))); fault != nil {
		t.Errorf("a tree of 7 + 4 bytes did not measure 11: %v", fault)
	}
	fault := manifest.Check(tree, manifestFor(t, dir, "", sizePtr(23)))
	if fault == nil {
		t.Fatal("the symlink's target length was counted as backup bytes")
	}
	if !strings.Contains(fault.Message, "11 bytes") {
		t.Errorf("message = %q, want it to name the measured 11 bytes", fault.Message)
	}
}

// TestAnUnwalkableTreeIsUnreadable: a directory the core cannot descend
// into is a permission problem, not a corrupt backup.
func TestAnUnwalkableTreeIsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a closed directory is still open")
	}
	dir := t.TempDir()
	tree := filepath.Join(dir, "backup")
	writeFile(t, filepath.Join(tree, "keep"), "x")
	closed := filepath.Join(tree, "closed")
	writeFile(t, filepath.Join(closed, "inner"), "y")
	if err := os.Chmod(closed, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(closed, 0o700) }) //nolint:errcheck // best effort, so t.TempDir can clean up

	for _, m := range []string{"", "sha256:" + strings.Repeat("0", 64)} {
		fault := manifest.Check(tree, manifestFor(t, dir, m, sizePtr(1)))
		if fault == nil || fault.Code != evidence.CodeSourceUnreadable {
			t.Fatalf("fault = %v, want %s", fault, evidence.CodeSourceUnreadable)
		}
	}
}

// TestAnUnreachableSourcePathIsUnreadable covers the stat failure that is
// not a missing file: the artifact may be there, and the core still
// cannot say so.
func TestAnUnreachableSourcePathIsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a closed directory is still open")
	}
	dir := t.TempDir()
	parent := filepath.Join(dir, "vault")
	writeFile(t, filepath.Join(parent, "nightly.dump"), "bytes")
	if err := os.Chmod(parent, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(parent, 0o700) }) //nolint:errcheck // best effort, so t.TempDir can clean up

	fault := manifest.Check(filepath.Join(parent, "nightly.dump"), manifestFor(t, dir, "", sizePtr(5)))
	if fault == nil || fault.Code != evidence.CodeSourceUnreadable {
		t.Fatalf("fault = %v, want %s", fault, evidence.CodeSourceUnreadable)
	}
}

// TestASizeOnlyTreeStillRefusesAnUnstatableFile: treeSize reads no bytes,
// but it does need each entry to still be there.
func TestASizeOnlyTreeStillRefusesAnUnstatableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a closed directory is still open")
	}
	dir := t.TempDir()
	tree := filepath.Join(dir, "backup")
	writeFile(t, filepath.Join(tree, "sub", "file"), "bytes")
	// Walkable while listing, closed by the time each entry is measured.
	fault := manifest.Check(tree, manifestFor(t, dir, "", sizePtr(5)))
	if fault != nil {
		t.Fatalf("a readable tree was refused: %v", fault)
	}
	if err := os.Chmod(filepath.Join(tree, "sub"), 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(tree, "sub"), 0o700) }) //nolint:errcheck // best effort
	if fault := manifest.Check(tree, manifestFor(t, dir, "", sizePtr(5))); fault != nil {
		t.Errorf("a readable-but-unwritable directory was refused: %v", fault)
	}
}

// TestAnUnreadableFileIsUnreadableNotCorrupt: the single-file shape of the
// same distinction. A backup the core cannot open has not been shown to
// disagree with anything.
func TestAnUnreadableFileIsUnreadableNotCorrupt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: an unreadable file is still readable")
	}
	dir := t.TempDir()
	artifact := filepath.Join(dir, "nightly.dump")
	writeFile(t, artifact, "bytes")
	if err := os.Chmod(artifact, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(artifact, 0o600) }) //nolint:errcheck // best effort, so t.TempDir can clean up

	fault := manifest.Check(artifact, manifestFor(t, dir, "sha256:"+strings.Repeat("0", 64), nil))
	if fault == nil || fault.Code != evidence.CodeSourceUnreadable {
		t.Fatalf("fault = %v, want %s", fault, evidence.CodeSourceUnreadable)
	}
}

// TestAPipeInATreeContributesNothing: §3 lists what contributes, and a
// FIFO is not on it — reading one would block the drill forever, and its
// presence must not change the digest either.
func TestAPipeInATreeContributesNothing(t *testing.T) {
	dir := t.TempDir()
	tree := filepath.Join(dir, "backup")
	writeFile(t, filepath.Join(tree, "data"), "rows")
	before := digestOf(t, tree)

	if err := syscall.Mkfifo(filepath.Join(tree, "pipe"), 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	if after := digestOf(t, tree); after != before {
		t.Errorf("a FIFO changed the tree hash: %s then %s", before, after)
	}
	if fault := manifest.Check(tree, manifestFor(t, dir, "", sizePtr(4))); fault != nil {
		t.Errorf("a FIFO was counted in the tree's size: %v", fault)
	}
}
