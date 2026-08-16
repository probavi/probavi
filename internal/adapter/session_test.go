package adapter

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestStartFailureIsAnError pins the race where the adapter executable
// disappears between resolution and exec: the operation must fail with a
// diagnosis naming the start, not hang or panic.
func TestStartFailureIsAnError(t *testing.T) {
	r := newRunner(filepath.Join(t.TempDir(), "gone"), nil, &Options{Grace: 500 * time.Millisecond})
	_, err := r.Probe(context.Background())
	if err == nil || !strings.Contains(err.Error(), "start adapter") {
		t.Fatalf("Probe = %v, want the start failure", err)
	}
}

// TestCloseStdinIsIdempotent: closeStdin runs on both the normal path and
// the finish() error path, so a second call must be a no-op and a failing
// Close (the pipe is often already gone with a dead process) must be
// absorbed, not escalated.
func TestCloseStdinIsIdempotent(t *testing.T) {
	_, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("pre-close: %v", err)
	}

	s := &session{stdin: w, logger: slog.New(slog.DiscardHandler)}
	s.closeStdin() // Close fails on the already-closed pipe; absorbed.
	if !s.stdinClosed {
		t.Error("closeStdin did not record the close")
	}
	s.closeStdin() // second call must return on the guard
}
