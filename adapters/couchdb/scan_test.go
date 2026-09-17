package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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

// wantUnreadable fails the test unless the refusal is the host saying it
// could not read something, in the words that name what.
func wantUnreadable(t *testing.T, perr *protoError, message string) {
	t.Helper()
	if perr == nil || perr.Code != "source_unreadable" || !strings.Contains(perr.Message, message) {
		t.Errorf("got %+v, want source_unreadable mentioning %q", perr, message)
	}
}

// TestTheDirectoryKindChoosesByTheHeaderLineAndTheClock pins how the kind
// that chooses for the operator chooses: a file is a candidate because it
// opens with couchbackup's header line, not because of its name, and the
// newest candidate wins with ties broken toward the later name.
func TestTheDirectoryKindChoosesByTheHeaderLineAndTheClock(t *testing.T) {
	dir := t.TempDir()
	old := writeArtifact(t, dir, "monday.txt", backupFixture(2))
	touchOlder(t, old)
	// Not candidates: a sidecar, a log, and a directory.
	writeArtifact(t, dir, "monday.txt.sha256", []byte("2b1f…  monday.txt\n"))
	writeArtifact(t, dir, "backup.log", []byte("couchbackup finished\n"))
	if err := os.MkdirAll(filepath.Join(dir, "archive"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := writeArtifact(t, dir, "tuesday.txt", backupFixture(3))

	src, perr := resolveSource(context.Background(), "couchbackup_dir", dir)
	if perr != nil {
		t.Fatalf("resolve: %+v", perr)
	}
	if src.path != want || src.batches != 3 {
		t.Errorf("chose %s (%d batches), want the newest candidate %s", src.path, src.batches, want)
	}

	// Written in the same second as the winner: the later name decides, so
	// a drill restores the same artifact every run.
	tie := writeArtifact(t, dir, "wednesday.txt", backupFixture(4))
	info, err := os.Stat(want)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tie, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if src, perr = resolveSource(context.Background(), "couchbackup_dir", dir); perr != nil || src.path != tie {
		t.Errorf("chose %+v (%+v), want the later name %s of two artifacts of one age", src, perr, tie)
	}
}

func TestTheDirectoryKindRefusesWhatHoldsNoBackup(t *testing.T) {
	onlyOthers := t.TempDir()
	writeArtifact(t, onlyOthers, "notes.txt", []byte("not a backup\n"))
	writeArtifact(t, onlyOthers, "backup.log", []byte("also not\n"))
	file := writeArtifact(t, t.TempDir(), "nightly.txt", backupFixture(1))
	for name, tc := range map[string]struct {
		path, code, message string
	}{
		"a directory that does not exist": {
			filepath.Join(t.TempDir(), "gone"), "source_not_found", "does not exist",
		},
		"a file": {file, "source_unreadable", "read backup directory"},
		"a directory with nothing in it": {
			t.TempDir(), "source_not_found", "contains no files",
		},
		"a directory holding no couchbackup file": {
			onlyOthers, "source_not_found", "holds no couchbackup files (2 files without its header line",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, perr := resolveSource(context.Background(), "couchbackup_dir", tc.path)
			if perr == nil || perr.Code != tc.code || !strings.Contains(perr.Message, tc.message) {
				t.Errorf("got %+v, want %s mentioning %q", perr, tc.code, tc.message)
			}
		})
	}
}

// TestAPathBeneathAFileIsUnreadable covers the stat failure every kind
// can meet: it is neither a missing source nor a wrong one, and a drill
// must not record it as something about the backup.
func TestAPathBeneathAFileIsUnreadable(t *testing.T) {
	file := writeArtifact(t, t.TempDir(), "nightly.txt", backupFixture(1))
	beneath := filepath.Join(file, "nightly.txt")
	for _, kind := range []string{"couchbackup", "couchbackup_dir", "couchdb_data", "couchdb_data_tar"} {
		t.Run(kind, func(t *testing.T) {
			_, perr := resolveSource(context.Background(), kind, beneath)
			if perr == nil || perr.Code != "source_unreadable" {
				t.Errorf("got %+v, want source_unreadable", perr)
			}
		})
	}
}

// TestBytesTheHostCannotReadAreUnreadable covers every read this adapter
// does on the drill host: the head it sniffs, the hash it streams, the
// tree it walks.
func TestBytesTheHostCannotReadAreUnreadable(t *testing.T) {
	t.Run("an artifact whose head will not be read", func(t *testing.T) {
		file := writeArtifact(t, t.TempDir(), "nightly.txt", backupFixture(1))
		closedTo(t, file, 0o600)
		_, perr := resolveSource(context.Background(), "couchbackup", file)
		wantUnreadable(t, perr, "read backup source")
	})
	t.Run("a candidate in a directory that will not be read", func(t *testing.T) {
		dir := t.TempDir()
		closedTo(t, writeArtifact(t, dir, "nightly.txt", backupFixture(1)), 0o600)
		_, perr := resolveSource(context.Background(), "couchbackup_dir", dir)
		wantUnreadable(t, perr, "read nightly.txt")
	})
	t.Run("bytes that will not stream", func(t *testing.T) {
		_, perr := fileChecksum(t.TempDir())
		wantUnreadable(t, perr, "read backup source")
	})
	t.Run("an artifact that will not open for hashing", func(t *testing.T) {
		_, perr := fileChecksum(filepath.Join(t.TempDir(), "gone.txt"))
		wantUnreadable(t, perr, "open backup source")
	})
	t.Run("a shard the host may not open", func(t *testing.T) {
		dir := dataDirFixture(t)
		closedTo(t, filepath.Join(dir, "shards", "00000000-7fffffff", "orders.1788098722.couch"), 0o600)
		_, perr := resolveSource(context.Background(), "couchdb_data", dir)
		wantUnreadable(t, perr, "open shards/")
	})
	t.Run("a directory the host may not walk", func(t *testing.T) {
		dir := dataDirFixture(t)
		closedTo(t, filepath.Join(dir, "shards"), 0o755)
		_, perr := resolveSource(context.Background(), "couchdb_data", dir)
		wantUnreadable(t, perr, "walk backup source")
	})
}

// TestTheBatchScanReadsLinesRatherThanBytes covers what the scan makes of
// the lines it meets: blank lines are neither a batch nor a torn tail, and
// a line longer than any batch may be stops the scan instead of buffering
// whatever the file chose.
func TestTheBatchScanReadsLinesRatherThanBytes(t *testing.T) {
	t.Run("blank lines count as nothing", func(t *testing.T) {
		body := append(backupFixture(2), []byte("\n   \n")...)
		body = append(body, backupFixture(1)[len(backupFixture(0)):]...)
		path := writeArtifact(t, t.TempDir(), "spaced.txt", body)
		batches, torn, err := batchLines(path)
		if err != nil || torn || batches != 3 {
			t.Errorf("batchLines = %d, %v, %v, want three batches and no torn tail", batches, torn, err)
		}
	})
	t.Run("a batch longer than the reader's buffer", func(t *testing.T) {
		long := append([]byte(`[{"_id":"big","_rev":"1-abc","payload":"`), bytes.Repeat([]byte("x"), 2<<20)...)
		long = append(long, []byte(`"}]`+"\n")...)
		path := writeArtifact(t, t.TempDir(), "long.txt", append(backupFixture(0), long...))
		batches, torn, err := batchLines(path)
		if err != nil || torn || batches != 1 {
			t.Errorf("batchLines = %d, %v, %v, want the long batch read whole", batches, torn, err)
		}
	})
	t.Run("a line no batch may be", func(t *testing.T) {
		huge := append(backupFixture(0), bytes.Repeat([]byte("x"), maxLineBytesScan+1)...)
		path := writeArtifact(t, t.TempDir(), "huge.txt", huge)
		if _, _, err := batchLines(path); err == nil || !strings.Contains(err.Error(), "exceeds the size") {
			t.Errorf("err = %v, want the scan to stop rather than buffer what the file chose", err)
		}
	})
	t.Run("a file that is not there", func(t *testing.T) {
		if _, _, err := batchLines(filepath.Join(t.TempDir(), "gone.txt")); err == nil {
			t.Error("batchLines read a file that does not exist")
		}
	})
}

// TestTheHeadReadStopsAtWhatItCanRead: readHead is the sniffing pass, and
// it answers with what the artifact holds rather than insisting on a full
// buffer.
func TestTheHeadReadStopsAtWhatItCanRead(t *testing.T) {
	short := writeArtifact(t, t.TempDir(), "short.txt", []byte("{}\n"))
	head, err := readHead(short, headMax)
	if err != nil || string(head) != "{}\n" {
		t.Errorf("readHead = %q, %v, want the three bytes the file holds", head, err)
	}
	// The bound is the caller's, and a file longer than it is read to it
	// and no further: the sniffing pass reads a head, never a backup.
	long := writeArtifact(t, t.TempDir(), "long.txt", backupFixture(4))
	if head, err = readHead(long, 8); err != nil || len(head) != 8 {
		t.Errorf("readHead = %q, %v, want exactly the eight bytes asked for", head, err)
	}
	if _, err := readHead(t.TempDir(), headMax); err == nil {
		t.Error("readHead read a directory as an artifact")
	}
}

// TestTheDirectoryScanRefusesAnArtifactInFlight proves the settle check is
// wired into the choice the adapter makes for the operator, and that it
// refuses rather than quietly restoring the older backup beside it.
func TestTheDirectoryScanRefusesAnArtifactInFlight(t *testing.T) {
	dir := t.TempDir()
	older := writeArtifact(t, dir, "a-monday.txt", backupFixture(2))
	touchOlder(t, older)
	newest := writeArtifact(t, dir, "z-in-flight.txt", backupFixture(1))

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			f, err := os.OpenFile(newest, os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				return
			}
			_, werr := f.WriteString(`[{"_id":"more","_rev":"1-abc"}]` + "\n")
			cerr := f.Close()
			if werr != nil || cerr != nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	_, perr := resolveSource(context.Background(), "couchbackup_dir", dir)
	close(stop)
	<-done

	if perr == nil || perr.Code != "source_unreadable" || !strings.Contains(perr.Message, "still being written") {
		t.Fatalf("got %+v, want source_unreadable about a backup in flight", perr)
	}
	if strings.Contains(perr.Message, "a-monday") {
		t.Error("the drill fell back to the older backup — that would prove a backup the record does not name")
	}
}
