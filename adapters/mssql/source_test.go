package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// touch sets a file's mtime so candidate ordering is deterministic.
func touch(t *testing.T, path string, ago time.Duration) {
	t.Helper()
	when := time.Now().Add(-ago)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

func TestResolveSourceKinds(t *testing.T) {
	dir := t.TempDir()
	old := writeMedia(t, dir, "a-old.bak", "OLD")
	latest := writeMedia(t, dir, "b-latest.bak", "NEW")
	touch(t, old, time.Hour)

	t.Run("bak names one artifact outright", func(t *testing.T) {
		plan, perr := resolveSource("bak", latest, nil)
		if perr != nil {
			t.Fatalf("resolve: %+v", perr)
		}
		if plan.fixed != latest || len(plan.candidates) != 0 {
			t.Errorf("plan = %+v, want the named artifact and no scan", plan)
		}
	})

	t.Run("bak_dir offers candidates newest first", func(t *testing.T) {
		plan, perr := resolveSource("bak_dir", dir, nil)
		if perr != nil {
			t.Fatalf("resolve: %+v", perr)
		}
		if plan.fixed != "" {
			t.Errorf("plan.fixed = %s, want a scan", plan.fixed)
		}
		want := []string{latest, old}
		if len(plan.candidates) != 2 || plan.candidates[0] != want[0] || plan.candidates[1] != want[1] {
			t.Errorf("candidates = %v, want newest first %v", plan.candidates, want)
		}
	})

	t.Run("unknown kind", func(t *testing.T) {
		if _, perr := resolveSource("pgdump", latest, nil); perr == nil || perr.Code != "unsupported_source" {
			t.Errorf("perr = %+v, want unsupported_source", perr)
		}
	})
}

// TestCandidateOrder pins the two rules a scan must follow: newest first,
// and — because mtimes collide on copied backup sets — the
// lexicographically larger name breaks a tie, so the choice never depends
// on directory iteration order.
func TestCandidateOrder(t *testing.T) {
	dir := t.TempDir()
	same := time.Now().Add(-time.Minute)
	for _, name := range []string{"a.bak", "b.bak"} {
		path := writeMedia(t, dir, name, name)
		if err := os.Chtimes(path, same, same); err != nil {
			t.Fatal(err)
		}
	}
	newest := writeMedia(t, dir, "0-newest.bak", "n")

	plan, perr := resolveSource("bak_dir", dir, nil)
	if perr != nil {
		t.Fatalf("resolve: %+v", perr)
	}
	want := []string{newest, filepath.Join(dir, "b.bak"), filepath.Join(dir, "a.bak")}
	for i := range want {
		if plan.candidates[i] != want[i] {
			t.Fatalf("candidates = %v, want %v", plan.candidates, want)
		}
	}
}

// TestCandidateScanSkipsNonMedia covers the sidecar problem: a checksum
// file or a log beside the backups must not be transferred and probed —
// measured, the engine answers "the volume ... is empty" for one, which
// would look exactly like a corrupt backup.
func TestCandidateScanSkipsNonMedia(t *testing.T) {
	dir := t.TempDir()
	backup := writeMedia(t, dir, "full.bak", "payload")
	touch(t, backup, time.Hour)
	for _, name := range []string{"SHA256SUMS", "backup.log"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("not a backup at all"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A compressed backup starts with a different magic; it is still media.
	compressed := filepath.Join(dir, "compressed.bak")
	if err := os.WriteFile(compressed, []byte("MSSQpayload"), 0o600); err != nil {
		t.Fatal(err)
	}

	plan, perr := resolveSource("bak_dir", dir, nil)
	if perr != nil {
		t.Fatalf("resolve: %+v", perr)
	}
	if len(plan.candidates) != 2 {
		t.Errorf("candidates = %v, want both backup media files", plan.candidates)
	}
	if plan.candidates[0] != compressed {
		t.Errorf("candidates = %v, want the compressed backup first (newest)", plan.candidates)
	}
	if len(plan.skipped) != 2 {
		t.Errorf("skipped = %v, want the two non-media files named for diagnostics", plan.skipped)
	}
}

func TestResolveSourceErrors(t *testing.T) {
	dir := t.TempDir()

	t.Run("missing file", func(t *testing.T) {
		if _, perr := resolveSource("bak", filepath.Join(dir, "gone"), nil); perr == nil || perr.Code != "source_not_found" {
			t.Errorf("perr = %+v, want source_not_found", perr)
		}
	})
	t.Run("directory for the file kind", func(t *testing.T) {
		if _, perr := resolveSource("bak", dir, nil); perr == nil || perr.Code != "invalid_request" {
			t.Errorf("perr = %+v, want invalid_request pointing at bak_dir", perr)
		}
	})
	t.Run("missing directory", func(t *testing.T) {
		if _, perr := resolveSource("bak_dir", filepath.Join(dir, "gone"), nil); perr == nil || perr.Code != "source_not_found" {
			t.Errorf("perr = %+v, want source_not_found", perr)
		}
	})
	t.Run("empty directory", func(t *testing.T) {
		empty := filepath.Join(dir, "empty")
		if err := os.Mkdir(empty, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, perr := resolveSource("bak_dir", empty, nil); perr == nil || perr.Code != "source_not_found" {
			t.Errorf("perr = %+v, want source_not_found", perr)
		}
	})
	t.Run("file for the dir kind", func(t *testing.T) {
		file := filepath.Join(dir, "file")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, perr := resolveSource("bak_dir", file, nil); perr == nil || perr.Code != "source_unreadable" {
			t.Errorf("perr = %+v, want source_unreadable", perr)
		}
	})
}

// TestIdentityOfChosenArtifact proves the backup identity describes what
// the engine chose, not what the host guessed first.
func TestIdentityOfChosenArtifact(t *testing.T) {
	dir := t.TempDir()
	older := writeMedia(t, dir, "full.bak", "FULL-PAYLOAD")
	touch(t, older, time.Hour)
	writeMedia(t, dir, "newest.trn", "LOG-PAYLOAD")

	plan, perr := resolveSource("bak_dir", dir, nil)
	if perr != nil {
		t.Fatalf("resolve: %+v", perr)
	}
	src, perr := plan.identity(older)
	if perr != nil {
		t.Fatalf("identity: %+v", perr)
	}
	if src.path != older {
		t.Errorf("path = %s, want the chosen artifact", src.path)
	}
	if src.sizeBytes != int64(len("TAPE"+"FULL-PAYLOAD")) {
		t.Errorf("sizeBytes = %d, want the chosen artifact's size", src.sizeBytes)
	}
	info, err := os.Stat(older)
	if err != nil {
		t.Fatal(err)
	}
	want := info.ModTime().UTC().Format("2006-01-02T15:04:05.000Z")
	if src.createdAt == nil || *src.createdAt != want {
		t.Errorf("createdAt = %v, want the chosen artifact's mtime %s", src.createdAt, want)
	}
	if src.loginsPath != "" {
		t.Errorf("loginsPath = %s, want empty for a plain directory", src.loginsPath)
	}
}

// withLoginsDir builds a two-member source directory: logins.sql (older)
// and orders.bak (newer).
func withLoginsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "logins.sql"), []byte("CREATE LOGIN"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeMedia(t, dir, "orders.bak", "BAK-BYTES")
	touch(t, filepath.Join(dir, "logins.sql"), time.Hour)
	return dir
}

func TestPlanWithLoginsErrors(t *testing.T) {
	dir := withLoginsDir(t)
	file := filepath.Join(t.TempDir(), "plain.bak")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		path     string
		params   map[string]string
		wantCode string
	}{
		{"missing params entirely", dir, nil, "invalid_request"},
		{"missing logins param", dir, map[string]string{"bak": "orders.bak"}, "invalid_request"},
		{"logins param is a path", dir, map[string]string{"logins": "../logins.sql"}, "invalid_request"},
		{"logins param is absolute", dir, map[string]string{"logins": "/etc/passwd"}, "invalid_request"},
		{"logins param is dot", dir, map[string]string{"logins": "."}, "invalid_request"},
		{"bak param is a path", dir, map[string]string{"logins": "logins.sql", "bak": "x/y.bak"}, "invalid_request"},
		{"both members name one file", dir, map[string]string{"logins": "logins.sql", "bak": "logins.sql"}, "invalid_request"},
		{"logins file missing", dir, map[string]string{"logins": "gone.sql"}, "source_not_found"},
		{"bak file missing", dir, map[string]string{"logins": "logins.sql", "bak": "gone.bak"}, "source_not_found"},
		{"logins member is a directory", dir, map[string]string{"logins": "sub"}, "invalid_request"},
		{"source path is a file", file, map[string]string{"logins": "logins.sql"}, "invalid_request"},
		{"source directory missing", filepath.Join(dir, "gone"), map[string]string{"logins": "logins.sql"}, "source_not_found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, perr := resolveSource("bak_with_logins", tt.path, tt.params); perr == nil || perr.Code != tt.wantCode {
				t.Errorf("perr = %+v, want %s", perr, tt.wantCode)
			}
		})
	}
}

func TestPlanWithLoginsScansForTheBackup(t *testing.T) {
	dir := withLoginsDir(t)

	t.Run("named backup skips the scan", func(t *testing.T) {
		plan, perr := resolveSource("bak_with_logins", dir, map[string]string{"logins": "logins.sql", "bak": "orders.bak"})
		if perr != nil {
			t.Fatalf("resolve: %+v", perr)
		}
		if plan.fixed != filepath.Join(dir, "orders.bak") || len(plan.candidates) != 0 {
			t.Errorf("plan = %+v, want the named backup", plan)
		}
	})

	t.Run("without one the directory is scanned, logins excluded", func(t *testing.T) {
		plan, perr := resolveSource("bak_with_logins", dir, map[string]string{"logins": "logins.sql"})
		if perr != nil {
			t.Fatalf("resolve: %+v", perr)
		}
		if len(plan.candidates) != 1 || plan.candidates[0] != filepath.Join(dir, "orders.bak") {
			t.Errorf("candidates = %v, want the backup alone", plan.candidates)
		}
		// The logins script is not backup media, but it is excluded by
		// name before that ever matters — it must not be reported skipped.
		for _, s := range plan.skipped {
			if s == "logins.sql" {
				t.Errorf("skipped = %v, want the named logins member excluded silently", plan.skipped)
			}
		}
	})
}

func TestCompositeIdentity(t *testing.T) {
	dir := withLoginsDir(t)
	params := map[string]string{"logins": "logins.sql", "bak": "orders.bak"}
	chosen := filepath.Join(dir, "orders.bak")

	plan, perr := resolveSource("bak_with_logins", dir, params)
	if perr != nil {
		t.Fatalf("resolve: %+v", perr)
	}
	src, perr := plan.identity(chosen)
	if perr != nil {
		t.Fatalf("identity: %+v", perr)
	}
	if src.path != chosen || src.loginsPath != filepath.Join(dir, "logins.sql") {
		t.Errorf("src = %+v", src)
	}
	if src.sizeBytes != int64(len("CREATE LOGIN")+len("TAPEBAK-BYTES")) {
		t.Errorf("sizeBytes = %d, want the sum of both members", src.sizeBytes)
	}

	// created_at is the OLDER member's mtime: the set is only as current
	// as its stalest member.
	older, err := os.Stat(filepath.Join(dir, "logins.sql"))
	if err != nil {
		t.Fatal(err)
	}
	want := older.ModTime().UTC().Format("2006-01-02T15:04:05.000Z")
	if src.createdAt == nil || *src.createdAt != want {
		t.Errorf("createdAt = %v, want the older member's mtime %s", src.createdAt, want)
	}

	again, perr := plan.identity(chosen)
	if perr != nil {
		t.Fatalf("identity again: %+v", perr)
	}
	if again.checksum != src.checksum {
		t.Errorf("checksum not deterministic: %s vs %s", again.checksum, src.checksum)
	}
}

func TestCompositeIdentityCreatedAtTracksStalestMember(t *testing.T) {
	dir := withLoginsDir(t)
	chosen := filepath.Join(dir, "orders.bak")
	touch(t, chosen, 2*time.Hour)

	plan, perr := resolveSource("bak_with_logins", dir, map[string]string{"logins": "logins.sql", "bak": "orders.bak"})
	if perr != nil {
		t.Fatalf("resolve: %+v", perr)
	}
	src, perr := plan.identity(chosen)
	if perr != nil {
		t.Fatalf("identity: %+v", perr)
	}
	info, err := os.Stat(chosen)
	if err != nil {
		t.Fatal(err)
	}
	want := info.ModTime().UTC().Format("2006-01-02T15:04:05.000Z")
	if src.createdAt == nil || *src.createdAt != want {
		t.Errorf("createdAt = %v, want the now-older backup mtime %s", src.createdAt, want)
	}
}

func TestCompositeIdentityCoversBothMembers(t *testing.T) {
	dir := withLoginsDir(t)
	params := map[string]string{"logins": "logins.sql", "bak": "orders.bak"}
	chosen := filepath.Join(dir, "orders.bak")
	plan, perr := resolveSource("bak_with_logins", dir, params)
	if perr != nil {
		t.Fatalf("resolve: %+v", perr)
	}
	base, perr := plan.identity(chosen)
	if perr != nil {
		t.Fatalf("identity: %+v", perr)
	}

	if err := os.WriteFile(filepath.Join(dir, "logins.sql"), []byte("CREATE LOGIM"), 0o600); err != nil {
		t.Fatal(err)
	}
	loginsChanged, perr := plan.identity(chosen)
	if perr != nil {
		t.Fatalf("identity: %+v", perr)
	}
	if loginsChanged.checksum == base.checksum {
		t.Error("checksum ignored a logins change — the identity must cover both members")
	}

	if err := os.WriteFile(filepath.Join(dir, "logins.sql"), []byte("CREATE LOGIN"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(chosen, []byte("TAPEBAK-BYTEZ"), 0o600); err != nil {
		t.Fatal(err)
	}
	bakChanged, perr := plan.identity(chosen)
	if perr != nil {
		t.Fatalf("identity: %+v", perr)
	}
	if bakChanged.checksum == base.checksum {
		t.Error("checksum ignored a backup change — the identity must cover both members")
	}
}

func TestCompositeIdentityIsUnambiguous(t *testing.T) {
	// "A"+"B" and "AB"+"" concatenate identically; the size framing must
	// keep their identities apart.
	newPlan := func(t *testing.T, logins, bak string) (*sourcePlan, string) {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "logins.sql"), []byte(logins), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "x.bak"), []byte(bak), 0o600); err != nil {
			t.Fatal(err)
		}
		plan, perr := resolveSource("bak_with_logins", dir, map[string]string{"logins": "logins.sql", "bak": "x.bak"})
		if perr != nil {
			t.Fatalf("resolve: %+v", perr)
		}
		return plan, filepath.Join(dir, "x.bak")
	}
	planA, chosenA := newPlan(t, "A", "B")
	planB, chosenB := newPlan(t, "AB", "")
	a, perr := planA.identity(chosenA)
	if perr != nil {
		t.Fatalf("identity a: %+v", perr)
	}
	b, perr := planB.identity(chosenB)
	if perr != nil {
		t.Fatalf("identity b: %+v", perr)
	}
	if a.checksum == b.checksum {
		t.Error("checksum collides across member boundaries — framing must include sizes")
	}
}

func TestCompositeIdentityIgnoresSiblings(t *testing.T) {
	dir := withLoginsDir(t)
	chosen := filepath.Join(dir, "orders.bak")
	plan, perr := resolveSource("bak_with_logins", dir, map[string]string{"logins": "logins.sql", "bak": "orders.bak"})
	if perr != nil {
		t.Fatalf("resolve: %+v", perr)
	}
	base, perr := plan.identity(chosen)
	if perr != nil {
		t.Fatalf("identity: %+v", perr)
	}
	// A half-written temp file beside the members must not change the
	// drill's backup identity.
	if err := os.WriteFile(filepath.Join(dir, "in-flight.tmp"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, perr := plan.identity(chosen)
	if perr != nil {
		t.Fatalf("identity: %+v", perr)
	}
	if after.checksum != base.checksum {
		t.Error("a sibling file changed the checksum — only the two members are the backup")
	}
}
