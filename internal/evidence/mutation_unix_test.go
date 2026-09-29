//go:build unix

package evidence

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestAWriteThatFailedLeavesNoKeyBehind: writeExclusive syncs only when the
// write succeeded, and the order matters more than it looks. Reversed, a
// failed write has its error replaced by the sync's — and syncing a file
// nothing was written to succeeds, so the function reports success and
// keeps the empty key file. A signing key that is not the key is worse than
// no key: the records naming its id already exist, so it cannot be rotated
// away from, and nobody can verify what it signed.
//
// This was recorded as unreachable and was not. The file is opened
// O_CREATE|O_EXCL at a path the caller names, and no ordinary filesystem
// fails the write that follows — but the failure does not have to come from
// the filesystem. RLIMIT_FSIZE is the process's own limit on how large a
// file it may write, and at zero every write to a regular file is refused
// with EFBIG while open, sync and close all still succeed. That is exactly
// the shape this branch is about, arranged without a seam, an injection or
// a parameter that exists for a test.
func TestAWriteThatFailedLeavesNoKeyBehind(t *testing.T) {
	// The directory is made before the limit is lowered: creating it is not
	// what this test is about.
	path := filepath.Join(t.TempDir(), "signing.key")

	var previous syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &previous); err != nil {
		t.Skipf("this host does not report its file-size limit: %v", err)
	}
	t.Cleanup(func() {
		if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &previous); err != nil {
			t.Errorf("restore the file-size limit: %v", err)
		}
	})
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 0, Max: previous.Max}); err != nil {
		t.Skipf("this host does not let a process lower its file-size limit: %v", err)
	}

	err := writeExclusive(path, "0123456789abcdef\n", 0o600)
	if err == nil {
		t.Fatal("writeExclusive reported success for a write that could not happen")
	}
	if !strings.Contains(err.Error(), "write "+path) {
		t.Errorf("err = %v, want it to name the write that failed rather than what followed", err)
	}
	if _, serr := os.Stat(path); !errors.Is(serr, fs.ErrNotExist) {
		t.Errorf("stat %s = %v, want it gone: a key that was never written must not survive its own failure",
			path, serr)
	}
}
