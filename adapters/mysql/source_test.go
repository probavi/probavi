package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveSourceFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "orders.sql")
	if err := os.WriteFile(path, []byte("-- dump"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	src, perr := resolveSource(context.Background(), "mysqldump", path, nil)
	if perr != nil {
		t.Fatalf("resolveSource: %+v", perr)
	}
	// The fixture carries no mysqldump trailer, so nothing dates it: the
	// file's mtime is deliberately not used, because copying a backup
	// resets it while leaving a perfectly valid backup behind.
	if !strings.HasPrefix(src.checksum, "sha256:") || src.sizeBytes != 7 || src.createdAt != nil {
		t.Errorf("src = %+v", src)
	}

	tests := []struct {
		name     string
		kind     string
		path     string
		wantCode string
	}{
		{"missing file", "mysqldump", filepath.Join(dir, "gone.sql"), "source_not_found"},
		{"directory as file", "mysqldump", dir, "invalid_request"},
		{"unsupported kind", "walg", path, "unsupported_source"},
		{"missing directory", "mysqldump_dir", filepath.Join(dir, "gone"), "source_not_found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, perr := resolveSource(context.Background(), tt.kind, tt.path, nil); perr == nil || perr.Code != tt.wantCode {
				t.Errorf("resolveSource(context.Background(), %s, %s) = %+v, want %s", tt.kind, tt.path, perr, tt.wantCode)
			}
		})
	}
}

func TestResolveSourceDirPicksNewest(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-time.Hour)
	for name, mtime := range map[string]time.Time{
		"monday.sql":  old.Add(-time.Hour),
		"tuesday.sql": old,
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}

	src, perr := resolveSource(context.Background(), "mysqldump_dir", dir, nil)
	if perr != nil {
		t.Fatalf("resolveSource: %+v", perr)
	}
	if filepath.Base(src.path) != "tuesday.sql" {
		t.Errorf("picked %s, want the newest file tuesday.sql", src.path)
	}

	// Equal mtimes: the lexicographically larger name must win so the
	// choice stays deterministic across runs.
	tie := filepath.Join(dir, "aaa.sql")
	if err := os.WriteFile(tie, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	newest := time.Now()
	for _, name := range []string{"tuesday.sql", "aaa.sql"} {
		if err := os.Chtimes(filepath.Join(dir, name), newest, newest); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}
	src, perr = resolveSource(context.Background(), "mysqldump_dir", dir, nil)
	if perr != nil {
		t.Fatalf("resolveSource: %+v", perr)
	}
	if filepath.Base(src.path) != "tuesday.sql" {
		t.Errorf("tie broke to %s, want tuesday.sql", src.path)
	}

	empty := t.TempDir()
	if _, perr := resolveSource(context.Background(), "mysqldump_dir", empty, nil); perr == nil || perr.Code != "source_not_found" {
		t.Errorf("empty dir: %+v, want source_not_found", perr)
	}
}

// withUsersDir builds a two-member source directory: users.sql (older)
// and orders.sql (newer).
func withUsersDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "users.sql"), []byte("CREATE USER"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "orders.sql"), []byte("DUMP-BYTES"), 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "users.sql"), past, past); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestResolveWithUsersErrors(t *testing.T) {
	dir := withUsersDir(t)
	file := filepath.Join(t.TempDir(), "plain.sql")
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
		{"missing users param", dir, map[string]string{"dump": "orders.sql"}, "invalid_request"},
		{"users param is a path", dir, map[string]string{"users": "../users.sql"}, "invalid_request"},
		{"users param is absolute", dir, map[string]string{"users": "/etc/passwd"}, "invalid_request"},
		{"users param is dot", dir, map[string]string{"users": "."}, "invalid_request"},
		{"dump param is a path", dir, map[string]string{"users": "users.sql", "dump": "x/y.sql"}, "invalid_request"},
		{"both members name one file", dir, map[string]string{"users": "users.sql", "dump": "users.sql"}, "invalid_request"},
		{"users file missing", dir, map[string]string{"users": "gone.sql"}, "source_not_found"},
		{"dump file missing", dir, map[string]string{"users": "users.sql", "dump": "gone.sql"}, "source_not_found"},
		{"users member is a directory", dir, map[string]string{"users": "sub"}, "invalid_request"},
		{"source path is a file", file, map[string]string{"users": "users.sql"}, "invalid_request"},
		{"source directory missing", filepath.Join(dir, "gone"), map[string]string{"users": "users.sql"}, "source_not_found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, perr := resolveSource(context.Background(), "mysqldump_with_users", tt.path, tt.params); perr == nil || perr.Code != tt.wantCode {
				t.Errorf("perr = %+v, want %s", perr, tt.wantCode)
			}
		})
	}

	t.Run("no dump beside the users script", func(t *testing.T) {
		lone := t.TempDir()
		if err := os.WriteFile(filepath.Join(lone, "users.sql"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, perr := resolveSource(context.Background(), "mysqldump_with_users", lone, map[string]string{"users": "users.sql"}); perr == nil || perr.Code != "source_not_found" {
			t.Errorf("perr = %+v, want source_not_found", perr)
		}
	})
}

func TestResolveWithUsersIdentity(t *testing.T) {
	dir := withUsersDir(t)
	params := map[string]string{"users": "users.sql", "dump": "orders.sql"}

	src, perr := resolveSource(context.Background(), "mysqldump_with_users", dir, params)
	if perr != nil {
		t.Fatalf("resolve: %+v", perr)
	}
	if src.path != filepath.Join(dir, "orders.sql") {
		t.Errorf("path = %s, want the dump member", src.path)
	}
	if src.usersPath != filepath.Join(dir, "users.sql") {
		t.Errorf("usersPath = %s", src.usersPath)
	}
	if src.sizeBytes != int64(len("CREATE USER")+len("DUMP-BYTES")) {
		t.Errorf("sizeBytes = %d, want the sum of both members", src.sizeBytes)
	}

	// Neither member is dated here: the fixture carries no mysqldump
	// trailer, and an accounts script is operator-authored and never
	// carries one. The set's age used to be the older member's mtime, a
	// rule that only ever worked while mtimes were trusted; nothing
	// replaces it with a guess.
	if src.createdAt != nil {
		t.Errorf("createdAt = %v, want none", *src.createdAt)
	}

	again, perr := resolveSource(context.Background(), "mysqldump_with_users", dir, params)
	if perr != nil {
		t.Fatalf("resolve again: %+v", perr)
	}
	if again.checksum != src.checksum {
		t.Errorf("checksum not deterministic: %s vs %s", again.checksum, src.checksum)
	}
}

// TestResolveWithUsersIsDatedByTheDump proves what now dates a two-member
// source: the dump's own trailer, which is the only member that carries a
// timestamp at all.
func TestResolveWithUsersIsDatedByTheDump(t *testing.T) {
	dir := withUsersDir(t)
	dump := filepath.Join(dir, "orders.sql")
	if err := os.WriteFile(dump, []byte("INSERT ...\n-- Dump completed on 2026-08-09 21:08:17\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src, perr := resolveSource(context.Background(), "mysqldump_with_users", dir,
		map[string]string{"users": "users.sql", "dump": "orders.sql", backupTimezoneParam: "Asia/Tokyo"})
	if perr != nil {
		t.Fatalf("resolve: %+v", perr)
	}
	if src.createdAt == nil || *src.createdAt != "2026-08-09T21:08:17.000+09:00" {
		t.Errorf("createdAt = %v, want the dump's own completion time in the declared zone", src.createdAt)
	}
}

func TestResolveWithUsersChecksumCoversBothMembers(t *testing.T) {
	dir := withUsersDir(t)
	params := map[string]string{"users": "users.sql", "dump": "orders.sql"}
	base, perr := resolveSource(context.Background(), "mysqldump_with_users", dir, params)
	if perr != nil {
		t.Fatalf("resolve: %+v", perr)
	}

	if err := os.WriteFile(filepath.Join(dir, "users.sql"), []byte("CREATE USERZ"), 0o600); err != nil {
		t.Fatal(err)
	}
	usersChanged, perr := resolveSource(context.Background(), "mysqldump_with_users", dir, params)
	if perr != nil {
		t.Fatalf("resolve: %+v", perr)
	}
	if usersChanged.checksum == base.checksum {
		t.Error("checksum ignored a users change — the identity must cover both members")
	}

	if err := os.WriteFile(filepath.Join(dir, "users.sql"), []byte("CREATE USER"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "orders.sql"), []byte("DUMP-BYTEZ"), 0o600); err != nil {
		t.Fatal(err)
	}
	dumpChanged, perr := resolveSource(context.Background(), "mysqldump_with_users", dir, params)
	if perr != nil {
		t.Fatalf("resolve: %+v", perr)
	}
	if dumpChanged.checksum == base.checksum {
		t.Error("checksum ignored a dump change — the identity must cover both members")
	}
}

func TestResolveWithUsersChecksumIsUnambiguous(t *testing.T) {
	// "A"+"B" and "AB"+"" concatenate identically; the size framing must
	// keep their identities apart.
	dirA := t.TempDir()
	if err := os.WriteFile(filepath.Join(dirA, "users.sql"), []byte("A"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirA, "x.sql"), []byte("B"), 0o600); err != nil {
		t.Fatal(err)
	}
	dirB := t.TempDir()
	if err := os.WriteFile(filepath.Join(dirB, "users.sql"), []byte("AB"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirB, "x.sql"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	params := map[string]string{"users": "users.sql", "dump": "x.sql"}
	a, perr := resolveSource(context.Background(), "mysqldump_with_users", dirA, params)
	if perr != nil {
		t.Fatalf("resolve a: %+v", perr)
	}
	b, perr := resolveSource(context.Background(), "mysqldump_with_users", dirB, params)
	if perr != nil {
		t.Fatalf("resolve b: %+v", perr)
	}
	if a.checksum == b.checksum {
		t.Error("checksum collides across member boundaries — framing must include sizes")
	}
}

func TestResolveWithUsersIgnoresSiblings(t *testing.T) {
	dir := withUsersDir(t)
	params := map[string]string{"users": "users.sql", "dump": "orders.sql"}
	base, perr := resolveSource(context.Background(), "mysqldump_with_users", dir, params)
	if perr != nil {
		t.Fatalf("resolve: %+v", perr)
	}
	// A half-written temp file beside the members must not change the
	// drill's backup identity.
	if err := os.WriteFile(filepath.Join(dir, "in-flight.tmp"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, perr := resolveSource(context.Background(), "mysqldump_with_users", dir, params)
	if perr != nil {
		t.Fatalf("resolve: %+v", perr)
	}
	if after.checksum != base.checksum {
		t.Error("a sibling file changed the checksum — only the two members are the backup")
	}
}

func TestResolveWithUsersPicksNewestNonUsers(t *testing.T) {
	dir := withUsersDir(t)
	// Make the users script the newest file in the directory: the implicit
	// dump choice must still skip it.
	now := time.Now()
	if err := os.Chtimes(filepath.Join(dir, "users.sql"), now, now); err != nil {
		t.Fatal(err)
	}
	past := now.Add(-time.Minute)
	if err := os.Chtimes(filepath.Join(dir, "orders.sql"), past, past); err != nil {
		t.Fatal(err)
	}
	src, perr := resolveSource(context.Background(), "mysqldump_with_users", dir, map[string]string{"users": "users.sql"})
	if perr != nil {
		t.Fatalf("resolve: %+v", perr)
	}
	if src.path != filepath.Join(dir, "orders.sql") {
		t.Errorf("path = %s, want the newest non-users file", src.path)
	}
}
