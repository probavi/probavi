package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// headerFixture is the first 128 bytes of a real gbak backup, taken on
// Firebird 5.0.4. The clock inside it is the one gbak stamped.
const headerFixture = "testdata/header.fbk"

const fixtureClock = "Fri Aug 28 17:59:41 2026"

func TestReadBackupClockReadsTheEnginesOwnStamp(t *testing.T) {
	clock, err := readBackupClock(headerFixture)
	if err != nil {
		t.Fatalf("readBackupClock: %v", err)
	}
	if clock != fixtureClock {
		t.Errorf("clock = %q, want %q", clock, fixtureClock)
	}
}

// TestReadBackupClockRefusesWhatIsNotABackup: the header is identified by
// its magic, not by the file's name or extension. A drill pointed at the
// wrong file dates nothing rather than dating something wrong.
func TestReadBackupClockRefusesWhatIsNotABackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not.fbk")
	if err := os.WriteFile(path, []byte("this is not a gbak backup at all"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if clock, err := readBackupClock(path); err == nil {
		t.Errorf("readBackupClock = %q, want an error", clock)
	}
}

// TestCreatedAtNeedsADeclaredZone pins the rule the field exists for: the
// header carries a wall clock with no offset, so without a declared zone
// the adapter reports no creation time rather than inventing one. A
// backup recorded at the wrong instant is worse than one recorded at none.
func TestCreatedAtNeedsADeclaredZone(t *testing.T) {
	if got := createdAt(fixtureClock, nil); got != nil {
		t.Errorf("createdAt with no zone = %q, want nil", *got)
	}
	utc, perr := backupLocation(map[string]string{backupTimezoneParam: "UTC"})
	if perr != nil {
		t.Fatalf("backupLocation: %v", perr)
	}
	got := createdAt(fixtureClock, utc)
	if got == nil {
		t.Fatal("createdAt with a zone = nil, want an instant")
	}
	if want := "2026-08-28T17:59:41.000Z"; *got != want {
		t.Errorf("createdAt = %q, want %q", *got, want)
	}
}

// TestCreatedAtHonoursTheDeclaredZone: the same wall clock is a different
// instant in a different zone, which is the whole reason the zone is
// declared rather than assumed.
func TestCreatedAtHonoursTheDeclaredZone(t *testing.T) {
	loc, perr := backupLocation(map[string]string{backupTimezoneParam: "Europe/Budapest"})
	if perr != nil {
		t.Fatalf("backupLocation: %v", perr)
	}
	got := createdAt(fixtureClock, loc)
	if got == nil {
		t.Fatal("createdAt = nil, want an instant")
	}
	if want := "2026-08-28T17:59:41.000+02:00"; *got != want {
		t.Errorf("createdAt = %q, want %q", *got, want)
	}
}

// TestBackupLocationRefusesAnUnknownZone: a misspelled zone fails the
// drill instead of silently dropping the timestamp it was meant to anchor.
func TestBackupLocationRefusesAnUnknownZone(t *testing.T) {
	_, perr := backupLocation(map[string]string{backupTimezoneParam: "Europe/Budapesst"})
	if perr == nil {
		t.Fatal("backupLocation accepted a zone that does not exist")
	}
	if perr.Code != "invalid_request" {
		t.Errorf("code = %s, want invalid_request", perr.Code)
	}
}

// fixtureIn writes a real gbak header under the given name, so what these
// tests exercise is source resolution rather than a header parse failing
// for an unrelated reason.
func fixtureIn(t *testing.T, dir, name string) string {
	t.Helper()
	body, err := os.ReadFile(headerFixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// TestResolveSourceAcceptsBothKinds: one named backup, and a directory the
// adapter chooses from. Both must produce a sha256 reference over the
// bytes that will be restored.
func TestResolveSourceAcceptsBothKinds(t *testing.T) {
	dir := t.TempDir()
	backup := fixtureIn(t, dir, "one.fbk")
	info, err := os.Stat(backup)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	for _, tc := range []struct{ name, kind, path string }{
		{"one backup", "firebird_gbak", backup},
		{"a directory of them", "firebird_gbak_dir", dir},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src, perr := resolveSource(context.Background(), tc.kind, tc.path, nil)
			if perr != nil {
				t.Fatalf("resolveSource: %v", perr)
			}
			if src.sizeBytes != info.Size() {
				t.Errorf("size = %d, want %d", src.sizeBytes, info.Size())
			}
			if len(src.checksum) != len("sha256:")+64 {
				t.Errorf("checksum %q is not a sha256 reference", src.checksum)
			}
		})
	}
}

// TestResolveSourceRefusals: every refusal carries the code the protocol
// registry defines for it, because the core's retry policy and the
// evidence record both read that code rather than the message.
func TestResolveSourceRefusals(t *testing.T) {
	dir := t.TempDir()
	backup := fixtureIn(t, dir, "one.fbk")

	tests := []struct {
		name     string
		kind     string
		path     string
		wantCode string
	}{
		{"a kind nobody declared", "firebird_dump", backup, "unsupported_source"},
		{"a path that is not there", "firebird_gbak", filepath.Join(dir, "gone.fbk"), "source_not_found"},
		{"a directory given as one backup", "firebird_gbak", dir, "invalid_request"},
		{"a directory that is not there", "firebird_gbak_dir", filepath.Join(dir, "gone"), "source_not_found"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, perr := resolveSource(context.Background(), tc.kind, tc.path, nil)
			if perr == nil {
				t.Fatalf("resolveSource succeeded, want %s", tc.wantCode)
			}
			if perr.Code != tc.wantCode {
				t.Errorf("code = %s (%s), want %s", perr.Code, perr.Message, tc.wantCode)
			}
		})
	}
}

// TestLatestBackupInPicksTheNewest, and breaks a tie deterministically so
// two runs of the same drill restore the same artifact.
func TestLatestBackupInPicksTheNewest(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "a.fbk")
	newer := filepath.Join(dir, "b.fbk")
	for _, p := range []string{old, newer} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	older := past.Add(-time.Hour)
	if err := os.Chtimes(newer, older.Add(time.Hour), older.Add(time.Hour)); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	// Both now share an mtime: the tie must break toward the larger name.
	got, perr := latestBackupIn(context.Background(), dir)
	if perr != nil {
		t.Fatalf("latestBackupIn: %v", perr)
	}
	if filepath.Base(got) != "b.fbk" {
		t.Errorf("chose %s, want b.fbk", filepath.Base(got))
	}
}

func TestLatestBackupInRefusesAnEmptyDirectory(t *testing.T) {
	_, perr := latestBackupIn(context.Background(), t.TempDir())
	if perr == nil || perr.Code != "source_not_found" {
		t.Fatalf("perr = %v, want source_not_found", perr)
	}
}
