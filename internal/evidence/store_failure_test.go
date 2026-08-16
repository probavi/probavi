package evidence

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The tests in this file force the store's I/O failure paths with real file
// descriptors in impossible states — closed, write-only, read-only, pipes —
// because the store's promise is that it fails loudly rather than continue
// appending onto a log whose tail state it cannot trust.

// brokenStore builds a Store around f without going through Open, so resume
// and closeTornTail can be driven against descriptors Open would never
// produce.
func brokenStore(f *os.File) *Store {
	return &Store{f: f, signer: testSigner(), logger: slog.New(slog.DiscardHandler)}
}

func TestOpenReportsAnUnsyncableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not bind root")
	}
	dir := filepath.Join(t.TempDir(), "logs")
	// Write+execute without read: files can be created inside, but the
	// directory itself cannot be opened for the durability sync.
	if err := os.Mkdir(dir, 0o300); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Errorf("restore dir mode: %v", err)
		}
	})

	_, err := Open(filepath.Join(dir, "evidence.jsonl"), testSigner(), nil)
	if err == nil || !strings.Contains(err.Error(), "evidence log directory") {
		t.Fatalf("Open = %v, want the directory-sync failure", err)
	}
}

func TestCloseReportsTheLockError(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "log"))
	if err != nil {
		t.Fatalf("create log: %v", err)
	}
	lock, err := os.Create(filepath.Join(dir, "lock"))
	if err != nil {
		t.Fatalf("create lock: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("pre-close lock: %v", err)
	}

	st := &Store{f: f, lock: lock}
	if err := st.Close(); err == nil || !strings.Contains(err.Error(), "close evidence store") {
		t.Fatalf("Close = %v, want the lock release failure to surface", err)
	}
}

// TestFsyncFailurePoisonsTheStore appends into a pipe, whose fsync always
// fails: the bytes left the process but their durability is unknown, so the
// store must refuse further appends until reopened.
func TestFsyncFailurePoisonsTheStore(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() {
		_ = r.Close() //nolint:errcheck // descriptor teardown only
		_ = w.Close() //nolint:errcheck // descriptor teardown only
	})

	st := brokenStore(w)
	st.nextSeq = 1
	st.prevHash = GenesisPrevHash

	if err := st.Append(sampleRecordPass()); err == nil || !strings.Contains(err.Error(), "fsync evidence log") {
		t.Fatalf("Append = %v, want the fsync failure", err)
	}
	if err := st.Append(sampleRecordPass()); err == nil || !strings.Contains(err.Error(), "failed state") {
		t.Fatalf("second Append = %v, want the poisoned-store refusal", err)
	}
}

// TestAppendRefusesAnUnrepresentableTiming: a timing beyond 2^53-1 passes
// structural validation (it is not negative) but cannot be canonicalized,
// so sealing fails — and must hand the record back with its chain fields
// cleared and the store still usable.
func TestAppendRefusesAnUnrepresentableTiming(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "evidence.jsonl"), testSigner(), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}()

	rec := sampleRecordPass()
	rec.Timings.Total = i64Ptr(MaxSafeInteger + 1)
	if err := st.Append(rec); !errors.Is(err, ErrNotInteger) {
		t.Fatalf("Append = %v, want ErrNotInteger", err)
	}
	if rec.Seq != 0 || rec.PrevHash != "" || rec.Sig != nil {
		t.Errorf("refused record kept chain fields: seq=%d prev=%q sig=%v", rec.Seq, rec.PrevHash, rec.Sig)
	}
	if err := st.Append(sampleRecordPass()); err != nil {
		t.Errorf("Append after a sealing refusal: %v — the store must remain usable", err)
	}
}

func TestResumeSurfacesUnreadableLogs(t *testing.T) {
	tempFile := func(t *testing.T, content string, flag int) *os.File {
		t.Helper()
		path := filepath.Join(t.TempDir(), "log")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		f, err := os.OpenFile(path, flag, 0o600)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() {
			_ = f.Close() //nolint:errcheck // descriptor teardown only
		})
		return f
	}

	tests := []struct {
		name string
		file func(t *testing.T) *os.File
		want string
	}{
		{"closed descriptor", func(t *testing.T) *os.File {
			f := tempFile(t, "", os.O_RDWR)
			if err := f.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
			return f
		}, "stat evidence log"},
		{"unseekable descriptor", func(t *testing.T) *os.File {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatalf("pipe: %v", err)
			}
			t.Cleanup(func() {
				_ = r.Close() //nolint:errcheck // descriptor teardown only
				_ = w.Close() //nolint:errcheck // descriptor teardown only
			})
			return w
		}, "seek evidence log"},
		{"write-only descriptor", func(t *testing.T) *os.File {
			return tempFile(t, "", os.O_WRONLY)
		}, "read evidence log"},
		{"write-only tail", func(t *testing.T) *os.File {
			return tempFile(t, "x", os.O_WRONLY)
		}, "read evidence log tail"},
		{"torn tail on a read-only log", func(t *testing.T) *os.File {
			return tempFile(t, "fragment-without-newline", os.O_RDONLY)
		}, "close torn tail"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := brokenStore(tt.file(t))
			if err := st.resume(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("resume = %v, want an error containing %q", err, tt.want)
			}
		})
	}
}
