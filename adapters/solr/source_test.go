package main

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// treeSize is what the artifact's regular files add up to, measured here
// rather than taken from the adapter under test.
func treeSize(t *testing.T, root string) int64 {
	t.Helper()
	var total int64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil || !d.Type().IsRegular() {
			return werr
		}
		info, err := d.Info()
		if err == nil {
			total += info.Size()
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return total
}

// datedAt sets a directory's own time, so a test says which of two
// backups is newer rather than hoping the clock cooperated.
func datedAt(t *testing.T, path string, when time.Time) string {
	t.Helper()
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestABackupNamesItsCollectionAndItsStartTime pins what a backup
// directory says about itself: the collection from its own layout, the
// instant from the engine's own properties file — unescaped, because
// Java's properties writer escapes the colons in it.
func TestABackupNamesItsCollectionAndItsStartTime(t *testing.T) {
	artifact := writeBackup(t, nil)
	src, perr := resolveSource(context.Background(), "solr_backup", artifact)
	if perr != nil {
		t.Fatalf("resolve: %+v", perr)
	}
	if src.path != artifact || src.collection != fixtureCollection || src.tarball {
		t.Errorf("resolved %+v, want the directory itself holding %s", src, fixtureCollection)
	}
	if src.createdAt == nil || *src.createdAt != "2026-08-27T18:34:34.622561925Z" {
		t.Errorf("created_at = %v, want the engine's own startTime", src.createdAt)
	}
	if !strings.HasPrefix(src.checksum, "sha256:") || src.sizeBytes != treeSize(t, artifact) {
		t.Errorf("identity = %s / %d, want a tree hash over %d bytes", src.checksum, src.sizeBytes, treeSize(t, artifact))
	}
}

// TestTheStartTimeIsTheArtifactsOrNothing: nothing is invented from a
// file's time, because a copied directory would date the copy and a wrong
// instant in a signed record is worse than an absent one.
func TestTheStartTimeIsTheArtifactsOrNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		properties map[string]string
		// remove is a properties file the artifact does not carry.
		remove string
		want   string
	}{
		"the engine's own instant": {want: "2026-08-27T18:34:34.622561925Z"},
		"the highest backup id in the location": {
			properties: map[string]string{"backup_1.properties": "startTime=2026-09-01T06\\:00\\:00Z\n"},
			want:       "2026-09-01T06:00:00Z",
		},
		// The id is a number, not a name: past the ninth backup in one
		// location a name comparison reads backup_9 as the latest, and the
		// record would date the drill from a backup the engine did not
		// restore.
		"an id past the ninth": {
			properties: map[string]string{
				"backup_9.properties":  "startTime=2026-09-09T09\\:00\\:00Z\n",
				"backup_10.properties": "startTime=2026-09-10T10\\:00\\:00Z\n",
			},
			want: "2026-09-10T10:00:00Z",
		},
		"a name the engine did not write": {
			properties: map[string]string{"backup_latest.properties": "startTime=2026-09-11T11\\:00\\:00Z\n"},
			want:       "2026-08-27T18:34:34.622561925Z",
		},
		"no properties file at all": {remove: "backup_0.properties"},
		"properties that record no start": {
			properties: map[string]string{"backup_0.properties": "backupName=nightly\ncollection=orders\n"},
		},
		"a start that is not an instant": {
			properties: map[string]string{"backup_0.properties": "startTime=yesterday evening\n"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			artifact := writeBackup(t, tc.properties)
			if tc.remove != "" {
				if err := os.Remove(filepath.Join(artifact, fixtureCollection, tc.remove)); err != nil {
					t.Fatal(err)
				}
			}
			got := backupStartTime(filepath.Join(artifact, fixtureCollection))
			if tc.want == "" {
				if got != nil {
					t.Errorf("created_at = %q, want none", *got)
				}
				return
			}
			if got == nil || *got != tc.want {
				t.Errorf("created_at = %v, want %q", got, tc.want)
			}
		})
	}
}

// TestAnArtifactThatIsNotOneBackupIsRefused: a backup of several
// collections is refused rather than guessed between, because restoring
// one of them would prove less than the backup holds and say nothing
// about it.
func TestAnArtifactThatIsNotOneBackupIsRefused(t *testing.T) {
	two := writeBackup(t, nil)
	if err := os.MkdirAll(filepath.Join(two, "invoices", "index"), 0o755); err != nil {
		t.Fatal(err)
	}
	noCollection := t.TempDir()
	noFiles := t.TempDir()
	if err := os.MkdirAll(filepath.Join(noFiles, "orders", "index"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		path, code, message string
	}{
		"a backup of two collections": {two, "unsupported_source", "holds 2 collections (invoices, orders)"},
		"a directory holding no collection": {
			noCollection, "source_corrupt", "holds no collection directory",
		},
		"a collection directory holding no file": {noFiles, "source_not_found", "contains no files"},
	} {
		t.Run(name, func(t *testing.T) {
			_, perr := resolveSource(context.Background(), "solr_backup", tc.path)
			if perr == nil || perr.Code != tc.code || !strings.Contains(perr.Message, tc.message) {
				t.Errorf("got %+v, want %s mentioning %q", perr, tc.code, tc.message)
			}
		})
	}
}

// TestTheTreeChecksumMeasuresPathsAndBytes: the hash is the evidence
// record's backup identity, so it has to change when the artifact does —
// including when only a name does.
func TestTheTreeChecksumMeasuresPathsAndBytes(t *testing.T) {
	artifact := writeBackup(t, nil)
	first, size, perr := treeChecksum(artifact)
	if perr != nil {
		t.Fatalf("checksum: %+v", perr)
	}
	again, _, perr := treeChecksum(artifact)
	if perr != nil || again != first {
		t.Errorf("the same tree hashed two ways: %s then %s (%+v)", first, again, perr)
	}
	index := filepath.Join(artifact, fixtureCollection, "index")
	if err := os.Rename(filepath.Join(index, "segments_1"), filepath.Join(index, "segments_2")); err != nil {
		t.Fatal(err)
	}
	moved, movedSize, perr := treeChecksum(artifact)
	if perr != nil || moved == first || movedSize != size {
		t.Errorf("renaming a file kept the sum (%v) or changed the size (%d vs %d)", moved == first, movedSize, size)
	}
}

func TestBytesTheHostCannotReadAreUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 file")
	}
	artifact := writeBackup(t, nil)
	target := filepath.Join(artifact, fixtureCollection, "index", "segments_1")
	if err := os.Chmod(target, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(target, 0o600); err != nil {
			t.Errorf("restore the mode: %v", err)
		}
	})
	_, perr := resolveSource(context.Background(), "solr_backup", artifact)
	if perr == nil || perr.Code != "source_unreadable" || !strings.Contains(perr.Message, "open ") {
		t.Errorf("got %+v, want source_unreadable naming the file", perr)
	}
}

// TestTheNewestBackupInADirectoryIsChosen pins the directory kind: the
// newest backup by its directory's time, ties broken toward the later
// name so a drill restores the same artifact every run, and nothing that
// is not a directory considered at all.
func TestTheNewestBackupInADirectoryIsChosen(t *testing.T) {
	now := time.Now()
	parent := t.TempDir()
	place := func(name string, when time.Time) string {
		path := filepath.Join(parent, name)
		if err := os.Rename(writeBackup(t, nil), path); err != nil {
			t.Fatal(err)
		}
		return datedAt(t, path, when)
	}
	place("monday", now.Add(-72*time.Hour))
	want := place("tuesday", now.Add(-48*time.Hour))
	if err := os.WriteFile(filepath.Join(parent, "zz-notes.txt"), []byte("not a backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	src, perr := resolveSource(context.Background(), "solr_backup_dir", parent)
	if perr != nil {
		t.Fatalf("resolve: %+v", perr)
	}
	if src.path != want || src.collection != fixtureCollection {
		t.Errorf("chose %s (%s), want the newest backup %s", src.path, src.collection, want)
	}

	tie := place("wednesday", now.Add(-48*time.Hour))
	if src, perr = resolveSource(context.Background(), "solr_backup_dir", parent); perr != nil || src.path != tie {
		t.Errorf("chose %+v (%+v), want the later name %s of two backups of one age", src, perr, tie)
	}
}

func TestDirectoryAndArchiveRefusalsNameWhatWasWrong(t *testing.T) {
	artifact := writeBackup(t, nil)
	parent := t.TempDir()
	file := filepath.Join(parent, "nightly.tar")
	if err := os.WriteFile(file, []byte("tar bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		kind, path, code, message string
	}{
		"an unknown source kind": {
			"solr_snapshot", artifact, "unsupported_source", "solr_backup_dir",
		},
		"a backup that does not exist": {
			"solr_backup", filepath.Join(parent, "gone"), "source_not_found", "does not exist",
		},
		"a file for the directory kind": {
			"solr_backup", file, "invalid_request", "is a file",
		},
		"a path beneath a file": {
			"solr_backup", filepath.Join(file, "orders"), "source_unreadable", "stat backup source",
		},
		"a directory of backups that does not exist": {
			"solr_backup_dir", filepath.Join(parent, "gone"), "source_not_found", "does not exist",
		},
		"a file for the kind that holds backups": {
			"solr_backup_dir", file, "source_unreadable", "read backup directory",
		},
		"a directory holding no backup": {
			"solr_backup_dir", t.TempDir(), "source_not_found", "contains no backups",
		},
		"an archive that does not exist": {
			"solr_backup_tar", filepath.Join(parent, "gone.tar"), "source_not_found", "does not exist",
		},
		"a directory for the archive kind": {
			"solr_backup_tar", artifact, "invalid_request", "use kind solr_backup",
		},
		"a path beneath a file for the archive kind": {
			"solr_backup_tar", filepath.Join(file, "nightly.tar"), "source_unreadable", "stat backup source",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, perr := resolveSource(context.Background(), tc.kind, tc.path)
			if perr == nil || perr.Code != tc.code || !strings.Contains(perr.Message, tc.message) {
				t.Errorf("got %+v, want %s mentioning %q", perr, tc.code, tc.message)
			}
		})
	}
}

// TestAnArchiveIsIdentifiedByItsOwnBytes: an archive moves as the file
// the drill named, so its identity is those bytes rather than a tree
// hash, and the collection comes from the one streaming pass over it.
func TestAnArchiveIsIdentifiedByItsOwnBytes(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "nightly.tar")
	body := tarSeed(t,
		"nightly/orders/backup_0.properties", "startTime=2026-08-27T18\\:34\\:34.622561925Z\n",
		"nightly/orders/index/segments_1", "index bytes",
	)
	if err := os.WriteFile(archive, body, 0o600); err != nil {
		t.Fatal(err)
	}
	src, perr := resolveSource(context.Background(), "solr_backup_tar", archive)
	if perr != nil {
		t.Fatalf("resolve: %+v", perr)
	}
	if !src.tarball || src.path != archive || src.collection != fixtureCollection {
		t.Errorf("resolved %+v, want the archive itself holding %s", src, fixtureCollection)
	}
	if src.sizeBytes != int64(len(body)) || !strings.HasPrefix(src.checksum, "sha256:") {
		t.Errorf("identity = %s / %d, want the archive's %d bytes", src.checksum, src.sizeBytes, len(body))
	}
	// An archive carries no directory to read a properties file out of, so
	// it dates to nothing until the sandbox unpacks it.
	if src.createdAt != nil {
		t.Errorf("created_at = %q, want none from an archive", *src.createdAt)
	}
}

// TestTheHashPassSaysWhatItCouldNotRead covers the artifact-reading
// failures a drill meets on the host: a file that is not there, a
// directory the host may not walk, and a path that opens but will not
// stream. None is the backup's fault, and the code says so.
func TestTheHashPassSaysWhatItCouldNotRead(t *testing.T) {
	t.Run("an archive that is not there", func(t *testing.T) {
		_, perr := fileChecksum(filepath.Join(t.TempDir(), "gone.tar"))
		if perr == nil || perr.Code != "source_unreadable" || !strings.Contains(perr.Message, "open ") {
			t.Errorf("got %+v, want source_unreadable", perr)
		}
	})
	t.Run("bytes that will not stream", func(t *testing.T) {
		_, perr := hashFile(io.Discard, t.TempDir())
		if perr == nil || perr.Code != "source_unreadable" || !strings.Contains(perr.Message, "read ") {
			t.Errorf("got %+v, want source_unreadable", perr)
		}
	})
	t.Run("a directory the host may not walk", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root walks a mode-000 directory")
		}
		artifact := writeBackup(t, nil)
		closed := filepath.Join(artifact, fixtureCollection, "index")
		if err := os.Chmod(closed, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.Chmod(closed, 0o755); err != nil {
				t.Errorf("restore the mode: %v", err)
			}
		})
		if _, _, perr := treeChecksum(artifact); perr == nil || perr.Code != "source_unreadable" ||
			!strings.Contains(perr.Message, "walk backup directory") {
			t.Errorf("got %+v, want source_unreadable", perr)
		}
	})
}

// TestTheBackupIdIsReadAsANumber covers the naming rule directly: only
// what the engine writes is a backup's record, and the id inside it is a
// number however many digits it has.
func TestTheBackupIdIsReadAsANumber(t *testing.T) {
	for name, want := range map[string]int{
		"backup_0.properties":   0,
		"backup_7.properties":   7,
		"backup_10.properties":  10,
		"backup_128.properties": 128,
	} {
		if id, ok := backupID(name); !ok || id != want {
			t.Errorf("backupID(%q) = %d, %v, want %d", name, id, ok, want)
		}
	}
	for _, name := range []string{
		"backup_.properties", "backup_latest.properties", "backup_-1.properties",
		"backup_0.properties.bak", "zk_backup_0", "shard_backup_metadata", "backup_1e3.properties",
	} {
		if id, ok := backupID(name); ok {
			t.Errorf("backupID(%q) = %d, true, want it refused as a name the engine did not write", name, id)
		}
	}
}
