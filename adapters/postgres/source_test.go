package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func touch(t *testing.T, dir, name string, mtime time.Time) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", name, err)
	}
	return path
}

func TestResolveSourceKinds(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	old := touch(t, dir, "a-old.dump", base)
	newest := touch(t, dir, "b-new.dump", base.Add(time.Hour))
	if err := os.Mkdir(filepath.Join(dir, "sub.dump"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	t.Run("pgdump file", func(t *testing.T) {
		src, perr := resolveSource(context.Background(), "pgdump", old, nil)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		if src.path != old || src.sizeBytes != int64(len("a-old.dump")) {
			t.Errorf("src = %+v", src)
		}
		// The fixture is not a custom-format archive, so nothing dates it:
		// the file's mtime is deliberately not used, because copying a
		// backup resets it while leaving a perfectly valid backup behind.
		if src.createdAt != nil {
			t.Errorf("createdAt = %v, want none — an mtime is not a backup timestamp", *src.createdAt)
		}
	})

	t.Run("pgdump_dir picks newest and ignores directories", func(t *testing.T) {
		src, perr := resolveSource(context.Background(), "pgdump_dir", dir, nil)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		if src.path != newest {
			t.Errorf("picked %s, want %s", src.path, newest)
		}
	})

	t.Run("pgdump_dir mtime tie breaks by name", func(t *testing.T) {
		tie := t.TempDir()
		touch(t, tie, "alpha.dump", base)
		zeta := touch(t, tie, "zeta.dump", base)
		src, perr := resolveSource(context.Background(), "pgdump_dir", tie, nil)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		if src.path != zeta {
			t.Errorf("picked %s, want deterministic tie-break to %s", src.path, zeta)
		}
	})

}

func TestResolveSourceErrors(t *testing.T) {
	dir := t.TempDir()
	touch(t, dir, "present.dump", time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC))

	base := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	// A well-formed with-globals source: a directory holding both members.
	set := t.TempDir()
	touch(t, set, "globals.sql", base)
	touch(t, set, "orders.dump", base)
	if err := os.Mkdir(filepath.Join(set, "nested"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A directory whose only file is the globals script.
	lonely := t.TempDir()
	touch(t, lonely, "globals.sql", base)

	withGlobals := func(entries ...string) map[string]string {
		p := map[string]string{"globals": "globals.sql"}
		if len(entries) > 0 {
			p["globals"] = entries[0]
		}
		if len(entries) > 1 {
			p["dump"] = entries[1]
		}
		return p
	}

	tests := []struct {
		name     string
		kind     string
		path     string
		params   map[string]string
		wantCode string
	}{
		{"unknown kind", "wal-g", dir, nil, "unsupported_source"},
		{"missing file", "pgdump", filepath.Join(dir, "gone.dump"), nil, "source_not_found"},
		{"directory as pgdump", "pgdump", dir, nil, "invalid_request"},
		{"missing directory", "pgdump_dir", filepath.Join(dir, "nodir"), nil, "source_not_found"},
		{"empty directory", "pgdump_dir", t.TempDir(), nil, "source_not_found"},

		{"with_globals without the params entry", "pgdump_with_globals", set,
			nil, "invalid_request"},
		{"with_globals with an empty params entry", "pgdump_with_globals", set,
			withGlobals(""), "invalid_request"},
		{"with_globals given a path instead of a name", "pgdump_with_globals", set,
			withGlobals("nested/globals.sql"), "invalid_request"},
		{"with_globals escaping the source directory", "pgdump_with_globals", set,
			withGlobals("../globals.sql"), "invalid_request"},
		{"with_globals given an absolute path", "pgdump_with_globals", set,
			withGlobals("/etc/passwd"), "invalid_request"},
		{"with_globals dump escaping the source directory", "pgdump_with_globals", set,
			withGlobals("globals.sql", "../orders.dump"), "invalid_request"},
		{"with_globals naming one file as both members", "pgdump_with_globals", set,
			withGlobals("globals.sql", "globals.sql"), "invalid_request"},
		{"with_globals missing globals script", "pgdump_with_globals", set,
			withGlobals("gone.sql"), "source_not_found"},
		{"with_globals missing named dump", "pgdump_with_globals", set,
			withGlobals("globals.sql", "gone.dump"), "source_not_found"},
		{"with_globals with nothing but the globals", "pgdump_with_globals", lonely,
			withGlobals(), "source_not_found"},
		{"with_globals missing directory", "pgdump_with_globals", filepath.Join(dir, "nodir"),
			withGlobals(), "source_not_found"},
		{"with_globals given a file as the source", "pgdump_with_globals",
			filepath.Join(dir, "present.dump"), withGlobals(), "invalid_request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, perr := resolveSource(context.Background(), tt.kind, tt.path, tt.params)
			if perr == nil || perr.Code != tt.wantCode {
				t.Errorf("resolveSource(context.Background(), %s, %s) = %+v, want %s", tt.kind, tt.path, perr, tt.wantCode)
			}
		})
	}
}

// The two members of a with-globals fixture, with the globals deliberately
// newer than the dump so a wrong created_at rule shows up.
var (
	globalsMtime = time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	dumpMtime    = time.Date(2026, 7, 29, 8, 0, 0, 0, time.UTC)
)

// writeAt writes body to dir/name and stamps it.
func writeAt(t *testing.T, dir, name, body string, mtime time.Time) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", name, err)
	}
}

// resolveSet builds a two-member source directory and resolves it.
func resolveSet(t *testing.T, globalsBody, dumpBody string) *resolvedSource {
	t.Helper()
	dir := t.TempDir()
	writeAt(t, dir, "globals.sql", globalsBody, globalsMtime)
	writeAt(t, dir, "orders.dump", dumpBody, dumpMtime)
	src, perr := resolveSource(context.Background(), "pgdump_with_globals", dir, map[string]string{"globals": "globals.sql"})
	if perr != nil {
		t.Fatalf("resolveSource: %+v", perr)
	}
	return src
}

// TestResolveWithGlobalsIdentity pins what the two-member source resolves
// to: the dump is restored, the globals are preloaded, and both count
// toward the size the evidence record carries.
func TestResolveWithGlobalsIdentity(t *testing.T) {
	src := resolveSet(t, "ROLES", "DUMP")
	if filepath.Base(src.path) != "orders.dump" || filepath.Base(src.globalsPath) != "globals.sql" {
		t.Errorf("src = %+v, want the dump restored and the globals preloaded", src)
	}
	if want := int64(len("ROLES") + len("DUMP")); src.sizeBytes != want {
		t.Errorf("sizeBytes = %d, want %d — both members count", src.sizeBytes, want)
	}
	// Neither member is dated here: the fixture is not a real archive, and
	// the globals script carries no timestamp of its own in any case
	// (measured — pg_dumpall --globals-only writes none). The set's age
	// used to be the older member's mtime, a rule that only ever worked
	// while mtimes were trusted; nothing replaces it with a guess.
	if src.createdAt != nil {
		t.Errorf("createdAt = %v, want none", *src.createdAt)
	}
}

// TestResolveWithGlobalsChecksumCoversBothMembers is the property the kind
// exists for: a checksum blind to the globals would let the roles a
// restore depends on change without the evidence record noticing.
func TestResolveWithGlobalsChecksumCoversBothMembers(t *testing.T) {
	base := resolveSet(t, "ROLES", "DUMP").checksum
	if got := resolveSet(t, "ROLES", "DUMP").checksum; got != base {
		t.Errorf("identical sets hashed differently: %s vs %s", got, base)
	}
	if got := resolveSet(t, "ROLES-CHANGED", "DUMP").checksum; got == base {
		t.Errorf("rewriting the globals left the checksum at %s", base)
	}
	if got := resolveSet(t, "ROLES", "DUMP-CHANGED").checksum; got == base {
		t.Errorf("rewriting the dump left the checksum at %s", base)
	}
}

// TestResolveWithGlobalsChecksumIsUnambiguous proves the construction is
// canonical rather than a concatenation. Without the size prefix, a
// globals script of "AB" beside an empty dump would be indistinguishable
// from "A" beside "B": two different backup sets sharing one identity.
func TestResolveWithGlobalsChecksumIsUnambiguous(t *testing.T) {
	split := resolveSet(t, "A", "B").checksum
	if joined := resolveSet(t, "AB", "").checksum; split == joined {
		t.Errorf("globals A + dump B hashes as globals AB + empty dump (%s) — "+
			"the member boundary is not encoded", split)
	}
}

// TestResolveWithGlobalsIgnoresSiblings keeps one drill's backup identity
// to what that drill restored.
//
// One directory may hold the globals beside several databases' dumps, each
// drilled separately. Hashing the directory instead of the chosen members
// would make every drill's checksum move whenever any other database was
// backed up — a changed backup identity that means nothing.
func TestResolveWithGlobalsIgnoresSiblings(t *testing.T) {
	dir := t.TempDir()
	writeAt(t, dir, "globals.sql", "ROLES", globalsMtime)
	writeAt(t, dir, "orders.dump", "DUMP", dumpMtime)
	writeAt(t, dir, "billing.dump", "OTHER", dumpMtime)
	params := map[string]string{"globals": "globals.sql", "dump": "orders.dump"}

	before, perr := resolveSource(context.Background(), "pgdump_with_globals", dir, params)
	if perr != nil {
		t.Fatalf("resolveSource: %+v", perr)
	}
	if want := resolveSet(t, "ROLES", "DUMP").checksum; before.checksum != want {
		t.Errorf("checksum = %s, want the same identity the two members have alone (%s)",
			before.checksum, want)
	}

	writeAt(t, dir, "billing.dump", "REDONE", dumpMtime)
	after, perr := resolveSource(context.Background(), "pgdump_with_globals", dir, params)
	if perr != nil {
		t.Fatalf("resolveSource: %+v", perr)
	}
	if after.checksum != before.checksum {
		t.Errorf("another database's backup moved this drill's checksum from %s to %s",
			before.checksum, after.checksum)
	}
}

// TestResolveWithGlobalsPicksNewestNonGlobals covers the unattended case:
// no params.dump, a rotating backup directory, and a globals script that
// happens to be the newest file of all — restoring it as the dump would be
// the obvious bug.
func TestResolveWithGlobalsPicksNewestNonGlobals(t *testing.T) {
	dir := t.TempDir()
	writeAt(t, dir, "globals.sql", "ROLES", globalsMtime.Add(time.Hour))
	writeAt(t, dir, "old.dump", "OLD", dumpMtime)
	writeAt(t, dir, "new.dump", "NEW", globalsMtime)

	src, perr := resolveSource(context.Background(), "pgdump_with_globals", dir, map[string]string{"globals": "globals.sql"})
	if perr != nil {
		t.Fatalf("resolveSource: %+v", perr)
	}
	if got := filepath.Base(src.path); got != "new.dump" {
		t.Errorf("picked %s, want new.dump — the globals must never be restored as the dump", got)
	}
}
