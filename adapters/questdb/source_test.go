package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestResolveSourceReadsWhatTheArtifactIs covers what each kind accepts,
// and what it read out of the tree.
func TestResolveSourceReadsWhatTheArtifactIs(t *testing.T) {
	checkpointed := writeDataRoot(t, dataRootOptions{checkpointed: true, tables: []string{"orders~9", "trades~11"}})
	plain := writeDataRoot(t, dataRootOptions{})

	tests := map[string]struct {
		kind, path string
		wantTables int
	}{
		"a checkpointed data root": {"questdb_checkpoint", checkpointed, 2},
		"the same root as data":    {"questdb_data", checkpointed, 2},
		"a plain copy as data":     {"questdb_data", plain, 1},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			src, perr := resolveSource(context.Background(), tc.kind, tc.path, nil)
			if perr != nil {
				t.Fatalf("resolveSource: %s: %s", perr.Code, perr.Message)
			}
			if src.userTables != tc.wantTables {
				t.Errorf("userTables = %d, want %d — the engine's own tables are not the operator's",
					src.userTables, tc.wantTables)
			}
			if !strings.HasPrefix(src.checksum, "sha256:") || src.sizeBytes == 0 {
				t.Errorf("identity = %q/%d, want a tree checksum and its size", src.checksum, src.sizeBytes)
			}
		})
	}
}

// TestResolveSourceRefusals covers what each kind will not take, and with
// which code — the half a drill reads when its configuration is wrong.
func TestResolveSourceRefusals(t *testing.T) {
	checkpointed := writeDataRoot(t, dataRootOptions{checkpointed: true})
	plain := writeDataRoot(t, dataRootOptions{})

	tests := map[string]struct {
		kind, path, wantCode string
	}{
		"a plain copy as a checkpoint": {"questdb_checkpoint", plain, "invalid_request"},
		"a file where a directory belongs": {
			"questdb_checkpoint", filepath.Join(checkpointed, "conf", "server.conf"), "invalid_request",
		},
		"a directory that is not a data root": {
			"questdb_checkpoint", writeDataRoot(t, dataRootOptions{noDB: true, checkpointed: true}), "source_corrupt",
		},
		"a path that does not exist": {"questdb_checkpoint", filepath.Join(t.TempDir(), "nope"), "source_not_found"},
		"an unknown kind":            {"questdb_snapshot", checkpointed, "unsupported_source"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			src, perr := resolveSource(context.Background(), tc.kind, tc.path, nil)
			if perr == nil {
				t.Fatalf("resolveSource = %+v, want %s", src, tc.wantCode)
			}
			if perr.Code != tc.wantCode {
				t.Errorf("code = %q, want %q (%s)", perr.Code, tc.wantCode, perr.Message)
			}
		})
	}
}

// TestCheckpointMarkerIsTheFilesNotTheDirectory pins the fence's basis.
// CHECKPOINT RELEASE empties .checkpoint and leaves the directory behind
// (measured on 10.0.1), so a directory test would call every released copy
// a checkpointed backup.
func TestCheckpointMarkerIsTheFilesNotTheDirectory(t *testing.T) {
	root := writeDataRoot(t, dataRootOptions{checkpointed: true})
	src, perr := resolveSource(context.Background(), "questdb_checkpoint", root, nil)
	if perr != nil || !src.checkpointed {
		t.Fatalf("a held checkpoint must be recognised: %+v %+v", src, perr)
	}
	// What RELEASE leaves: the directory, and nothing in it.
	if err := os.RemoveAll(filepath.Join(root, ".checkpoint", "db")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".checkpoint", "db"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, perr := resolveSource(context.Background(), "questdb_checkpoint", root, nil); perr == nil {
		t.Error("an emptied .checkpoint passed the fence — a released copy is not a checkpointed backup")
	}
}

// TestNewestDataRootIsChosen pins the directory kind. Nothing inside a
// QuestDB backup records when it was taken, so file time is the only date
// there is, and the adapter says so rather than implying better.
func TestNewestDataRootIsChosen(t *testing.T) {
	dir := t.TempDir()
	var newest string
	for i, name := range []string{"monday", "tuesday", "wednesday"} {
		root := writeDataRoot(t, dataRootOptions{checkpointed: true})
		dest := filepath.Join(dir, name)
		if err := os.Rename(root, dest); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(time.Duration(i-3) * time.Hour)
		if err := os.Chtimes(dest, when, when); err != nil {
			t.Fatal(err)
		}
		newest = dest
	}
	// A file and a directory that is not a data root are both ignored.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	src, perr := resolveSource(context.Background(), "questdb_checkpoint_dir", dir, nil)
	if perr != nil {
		t.Fatalf("resolveSource: %s", perr.Message)
	}
	if src.path != newest {
		t.Errorf("chose %s, want the newest %s", filepath.Base(src.path), filepath.Base(newest))
	}
}

// TestEmptyBackupDirectoryIsRefused keeps the directory kind from picking
// nothing quietly.
func TestEmptyBackupDirectoryIsRefused(t *testing.T) {
	_, perr := resolveSource(context.Background(), "questdb_checkpoint_dir", t.TempDir(), nil)
	if perr == nil || perr.Code != "source_not_found" {
		t.Fatalf("perr = %+v, want source_not_found", perr)
	}
}

// TestTreeChecksumFollowsTheBytes: the same tree hashes the same way, and
// a changed byte or a moved file changes the sum. The value reaches every
// signed evidence record as backup.checksum.
func TestTreeChecksumFollowsTheBytes(t *testing.T) {
	root := writeDataRoot(t, dataRootOptions{checkpointed: true})
	first, _, perr := treeChecksum(root)
	if perr != nil {
		t.Fatal(perr.Message)
	}
	again, _, _ := treeChecksum(root)
	if first != again {
		t.Errorf("checksum is not stable: %s vs %s", first, again)
	}
	column := filepath.Join(root, "db", "orders~9", "2026-09-01", "id.d")
	if err := os.WriteFile(column, []byte("different bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, _, _ := treeChecksum(root)
	if changed == first {
		t.Error("a changed column file left the checksum alone")
	}
}

// TestDirectoryKindRefusalsNameWhatWasWrong covers what the kind that
// chooses for the operator will not take.
func TestDirectoryKindRefusalsNameWhatWasWrong(t *testing.T) {
	file := filepath.Join(t.TempDir(), "nightly.tar")
	if err := os.WriteFile(file, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	notRoots := t.TempDir()
	if err := os.MkdirAll(filepath.Join(notRoots, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for name, tc := range map[string]struct {
		ctx                 context.Context
		path, code, message string
	}{
		"a directory that does not exist": {
			context.Background(), filepath.Join(t.TempDir(), "gone"), "source_not_found", "does not exist",
		},
		"a file": {context.Background(), file, "source_unreadable", "read backup directory"},
		"a directory holding no data root": {
			context.Background(), notRoots, "source_not_found", "holds no QuestDB data root",
		},
		"a drill cancelled while choosing": {cancelled, notRoots, "cancelled", "choosing a backup"},
	} {
		t.Run(name, func(t *testing.T) {
			_, perr := resolveSource(tc.ctx, "questdb_checkpoint_dir", tc.path, nil)
			if perr == nil || perr.Code != tc.code || !strings.Contains(perr.Message, tc.message) {
				t.Errorf("got %+v, want %s mentioning %q", perr, tc.code, tc.message)
			}
		})
	}
}

// wantUnreadable fails the test unless the refusal is the host saying it
// could not read something, in the words that name what.
func wantUnreadable(t *testing.T, perr *protoError, message string) {
	t.Helper()
	if perr == nil || perr.Code != "source_unreadable" || !strings.Contains(perr.Message, message) {
		t.Errorf("got %+v, want source_unreadable mentioning %q", perr, message)
	}
}

// closedTo makes a path unreadable for the rest of the test. Root reads a
// mode-000 path regardless, so the test is skipped there rather than
// asserting something the filesystem is not doing.
func closedTo(t *testing.T, path string, restore os.FileMode) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 path")
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(path, restore); err != nil {
			t.Errorf("restore the mode: %v", err)
		}
	})
}

// TestAnArtifactShapeTheHostCannotReadIsRefusedAsSuch: a stat or a listing
// that fails is neither a missing artifact nor a wrong one, and a drill
// must not record it as something about the backup.
func TestAnArtifactShapeTheHostCannotReadIsRefusedAsSuch(t *testing.T) {
	root := writeDataRoot(t, dataRootOptions{checkpointed: true})
	flat := filepath.Join(t.TempDir(), "flat")
	if err := os.MkdirAll(flat, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(flat, "db"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(t.TempDir(), "bare")
	if err := os.MkdirAll(filepath.Join(bare, "db"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		path, code, message string
	}{
		"a path beneath a file": {
			filepath.Join(root, "conf", "server.conf", "db"), "source_unreadable", "stat backup source",
		},
		"a db path that is not a directory":  {flat, "source_unreadable", "read db directory"},
		"a data root holding no file at all": {bare, "source_not_found", "contains no files"},
	} {
		t.Run(name, func(t *testing.T) {
			_, perr := resolveSource(context.Background(), "questdb_data", tc.path, nil)
			if perr == nil || perr.Code != tc.code || !strings.Contains(perr.Message, tc.message) {
				t.Errorf("got %+v, want %s mentioning %q", perr, tc.code, tc.message)
			}
		})
	}
}

// TestBytesTheHostCannotReadAreUnreadable covers every read the adapter
// does on the drill host: the checkpoint marker it lists, the column files
// it hashes, the tree it walks.
func TestBytesTheHostCannotReadAreUnreadable(t *testing.T) {
	t.Run("a checkpoint marker the host may not read", func(t *testing.T) {
		root := writeDataRoot(t, dataRootOptions{checkpointed: true})
		closedTo(t, filepath.Join(root, ".checkpoint", "db"), 0o755)
		_, perr := resolveSource(context.Background(), "questdb_checkpoint", root, nil)
		wantUnreadable(t, perr, "read checkpoint marker")
	})
	t.Run("a column file the host may not open", func(t *testing.T) {
		root := writeDataRoot(t, dataRootOptions{checkpointed: true})
		closedTo(t, filepath.Join(root, "db", "orders~9", "2026-09-01", "id.d"), 0o600)
		_, perr := resolveSource(context.Background(), "questdb_checkpoint", root, nil)
		wantUnreadable(t, perr, "open id.d")
	})
	t.Run("a directory the host may not walk", func(t *testing.T) {
		root := writeDataRoot(t, dataRootOptions{checkpointed: true})
		closedTo(t, filepath.Join(root, "public"), 0o755)
		_, _, perr := treeChecksum(root)
		wantUnreadable(t, perr, "walk backup directory")
	})
}
