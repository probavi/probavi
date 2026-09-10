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
			src, perr := resolveSource(context.Background(), tc.kind, tc.path)
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
			src, perr := resolveSource(context.Background(), tc.kind, tc.path)
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
	src, perr := resolveSource(context.Background(), "questdb_checkpoint", root)
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
	if _, perr := resolveSource(context.Background(), "questdb_checkpoint", root); perr == nil {
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
	src, perr := resolveSource(context.Background(), "questdb_checkpoint_dir", dir)
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
	_, perr := resolveSource(context.Background(), "questdb_checkpoint_dir", t.TempDir())
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
