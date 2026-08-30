package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeSnapshot writes an artifact and backdates it past the settle
// window, so a directory scan treats it as a finished backup.
func writeSnapshot(t *testing.T, dir, name string, body []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("backdate %s: %v", name, err)
	}
	return path
}

// writeSidecar writes the .checksum file Qdrant leaves beside every
// snapshot: the bare SHA-256 of the file, lowercase hex.
func writeSidecar(t *testing.T, snapshot string, digest string) {
	t.Helper()
	if err := os.WriteFile(snapshot+checksumSuffix, []byte(digest+"\n"), 0o600); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
}

func digestOf(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func TestResolveSourceAcceptsEveryKind(t *testing.T) {
	dir := t.TempDir()
	body := []byte("a qdrant snapshot is an ordinary tar")
	one := writeSnapshot(t, dir, "orders-1-2026-08-30.snapshot", body)

	nested := filepath.Join(dir, "nest")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeSnapshot(t, nested, "old.snapshot", []byte("older"))
	newest := writeSnapshot(t, nested, "new.snapshot", body)
	later := time.Now().Add(-time.Minute)
	if err := os.Chtimes(newest, later, later); err != nil {
		t.Fatalf("touch: %v", err)
	}

	for _, tc := range []struct {
		kind, path string
		wantPath   string
		wantForm   sourceForm
	}{
		{"qdrant_snapshot", one, one, formSnapshot},
		{"qdrant_snapshot_dir", nested, newest, formSnapshot},
		{"qdrant_full_snapshot", one, one, formFullSnapshot},
		{"qdrant_full_snapshot_dir", nested, newest, formFullSnapshot},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			src, perr := resolveSource(context.Background(), tc.kind, tc.path)
			if perr != nil {
				t.Fatalf("resolve: %+v", perr)
			}
			if src.path != tc.wantPath {
				t.Errorf("resolved %s, want %s", src.path, tc.wantPath)
			}
			if src.form != tc.wantForm {
				t.Errorf("form %s, want %s", src.form, tc.wantForm)
			}
			if src.checksum != "sha256:"+digestOf(body) {
				t.Errorf("checksum %s does not measure the artifact's bytes", src.checksum)
			}
		})
	}
}

// TestTheLeadingKindReadsNoMagicBytes is the conformance suite's shape as
// a unit test, and it is a design decision rather than an accident: the
// generated source is 64 KB of random bytes, and check 9 requires
// provision to succeed against it. Any host-side content refusal on the
// first declared kind would fail conformance at whatever rate those bytes
// happened to look like the thing being refused — and there is no need for
// one, because Qdrant refuses a damaged snapshot itself, hard.
func TestTheLeadingKindReadsNoMagicBytes(t *testing.T) {
	dir := t.TempDir()
	body := make([]byte, 64*1024)
	if _, err := rand.Read(body); err != nil {
		t.Fatalf("random: %v", err)
	}
	// The two shapes a content sniffer would most likely reject.
	for name, head := range map[string][]byte{
		"random": nil,
		"gzip":   {0x1f, 0x8b},
		"zeroes": make([]byte, 16),
	} {
		t.Run(name, func(t *testing.T) {
			b := append([]byte(nil), body...)
			copy(b, head)
			path := writeSnapshot(t, dir, name+".snapshot", b)
			if _, perr := resolveSource(context.Background(), "qdrant_snapshot", path); perr != nil {
				t.Errorf("the leading kind refused an artifact on its content: %+v", perr)
			}
		})
	}
}

// TestTheChecksumSidecarIsVerified is the one fence that fires before a
// byte crosses into the sandbox. Qdrant writes the digest beside every
// snapshot, and it matches the file exactly (measured), so an artifact
// that has changed since it was taken can be named as such rather than
// handed to the engine.
func TestTheChecksumSidecarIsVerified(t *testing.T) {
	body := []byte("the snapshot as Qdrant wrote it")

	t.Run("a matching sidecar passes and is reported", func(t *testing.T) {
		dir := t.TempDir()
		path := writeSnapshot(t, dir, "a.snapshot", body)
		writeSidecar(t, path, digestOf(body))
		src, perr := resolveSource(context.Background(), "qdrant_snapshot", path)
		if perr != nil {
			t.Fatalf("resolve: %+v", perr)
		}
		if src.declaredChecksum != digestOf(body) {
			t.Errorf("declaredChecksum %q, want the sidecar's digest", src.declaredChecksum)
		}
	})

	t.Run("a mismatching sidecar refuses the artifact", func(t *testing.T) {
		dir := t.TempDir()
		path := writeSnapshot(t, dir, "b.snapshot", body)
		writeSidecar(t, path, digestOf([]byte("something else entirely")))
		_, perr := resolveSource(context.Background(), "qdrant_snapshot", path)
		if perr == nil {
			t.Fatal("an artifact that does not match its own checksum was accepted")
		}
		if perr.Code != "source_corrupt" {
			t.Errorf("code %q, want source_corrupt", perr.Code)
		}
	})

	t.Run("a malformed sidecar is not treated as an absent one", func(t *testing.T) {
		dir := t.TempDir()
		path := writeSnapshot(t, dir, "c.snapshot", body)
		writeSidecar(t, path, "not a digest")
		_, perr := resolveSource(context.Background(), "qdrant_snapshot", path)
		if perr == nil {
			t.Fatal("a sidecar that states nothing was silently ignored")
		}
		if perr.Code != "source_corrupt" {
			t.Errorf("code %q, want source_corrupt", perr.Code)
		}
	})

	t.Run("no sidecar leaves the engine as the only judge", func(t *testing.T) {
		dir := t.TempDir()
		path := writeSnapshot(t, dir, "d.snapshot", body)
		src, perr := resolveSource(context.Background(), "qdrant_snapshot", path)
		if perr != nil {
			t.Fatalf("a snapshot copied without its sidecar must still be restorable: %+v", perr)
		}
		if src.declaredChecksum != "" {
			t.Errorf("declaredChecksum %q, want empty", src.declaredChecksum)
		}
	})
}

// TestTheSidecarIsNotItselfACandidate keeps a directory scan from
// restoring the 64-byte digest file instead of the snapshot beside it.
func TestTheSidecarIsNotItselfACandidate(t *testing.T) {
	dir := t.TempDir()
	body := []byte("the snapshot")
	snap := writeSnapshot(t, dir, "orders.snapshot", body)
	writeSidecar(t, snap, digestOf(body))
	// The sidecar is the newer file, so a scan ranking by time alone
	// would pick it.
	src, perr := resolveSource(context.Background(), "qdrant_snapshot_dir", dir)
	if perr != nil {
		t.Fatalf("resolve: %+v", perr)
	}
	if src.path != snap {
		t.Errorf("directory scan chose %s, want %s", filepath.Base(src.path), filepath.Base(snap))
	}
}

func TestResolveSourceRefusals(t *testing.T) {
	dir := t.TempDir()
	good := writeSnapshot(t, dir, "ok.snapshot", []byte("body"))
	empty := writeSnapshot(t, dir, "empty.snapshot", nil)

	noSnapshots := filepath.Join(dir, "logs")
	if err := os.Mkdir(noSnapshots, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeSnapshot(t, noSnapshots, "backup.log", []byte("a log, not a snapshot"))

	emptyDir := filepath.Join(dir, "nothing")
	if err := os.Mkdir(emptyDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	for _, tc := range []struct {
		name, kind, path, wantCode, wantSays string
	}{
		{"unknown kind", "qdrant_dump", good, "unsupported_source", "supported"},
		{"missing file", "qdrant_snapshot", filepath.Join(dir, "gone.snapshot"), "source_not_found", ""},
		{"directory for the file kind", "qdrant_snapshot", dir, "invalid_request", "qdrant_snapshot_dir"},
		{"directory for the full file kind", "qdrant_full_snapshot", dir, "invalid_request", "qdrant_full_snapshot_dir"},
		{"empty artifact", "qdrant_snapshot", empty, "source_corrupt", "0 bytes"},
		{"missing directory", "qdrant_snapshot_dir", filepath.Join(dir, "gone"), "source_not_found", ""},
		{"directory with no snapshots", "qdrant_snapshot_dir", noSnapshots, "source_not_found", "passed over"},
		{"empty directory", "qdrant_snapshot_dir", emptyDir, "source_not_found", "no files"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, perr := resolveSource(context.Background(), tc.kind, tc.path)
			if perr == nil {
				t.Fatal("expected a refusal")
			}
			if perr.Code != tc.wantCode {
				t.Errorf("code %q, want %q (%s)", perr.Code, tc.wantCode, perr.Message)
			}
			if tc.wantSays != "" && !strings.Contains(perr.Message, tc.wantSays) {
				t.Errorf("message %q does not say %q", perr.Message, tc.wantSays)
			}
		})
	}
}
