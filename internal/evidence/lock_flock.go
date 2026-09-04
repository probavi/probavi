//go:build unix && !aix && (!solaris || illumos)

package evidence

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// tryLockExclusive takes a non-blocking advisory lock on the whole file,
// or reports errLockHeld when another writer already has it.
//
// flock is the right primitive rather than the convenient one: the lock
// belongs to the open file description, so it is released when the
// process exits however it exits — including a kill -9 mid-drill, which
// is exactly the case a lock file left on disk cannot recover from.
func tryLockExclusive(f *os.File) error {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return errLockHeld
		}
		return fmt.Errorf("flock: %w", err)
	}
	return nil
}
