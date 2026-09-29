// Package cli provides the subprocess plumbing shared by sandbox providers
// that shell out to an already-verified host binary (docker, kubectl)
// instead of dragging an SDK module tree into a trust product.
package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/probavi/probavi/internal/sandbox"
)

// Runner abstracts subprocess execution so provider logic is unit-testable
// without the real CLI; ExecRunner is the real implementation.
type Runner interface {
	// Run executes name with args. err reports spawn/context failures only;
	// a non-zero exit code is returned in exitCode with err == nil.
	//
	// env entries ("NAME=value") are added to the child's environment on top
	// of the parent's. They exist so a secret can reach a CLI without
	// appearing in its argv, where every local user could read it out of the
	// process list; nil means "inherit and nothing more".
	Run(ctx context.Context, stdin io.Reader, env []string, name string, args ...string) (stdout, stderr []byte, truncated bool, exitCode int, err error)
}

// ExecRunner runs commands with os/exec.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, stdin io.Reader, env []string, name string, args ...string) ([]byte, []byte, bool, int, error) {
	var out, errOut limitedWriter
	cmd := exec.CommandContext(ctx, name, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	cmd.Stdin = stdin
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	truncated := out.truncated || errOut.truncated
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return out.buf.Bytes(), errOut.buf.Bytes(), truncated, exitErr.ExitCode(), nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return out.buf.Bytes(), errOut.buf.Bytes(), truncated, 0, fmt.Errorf("%s: %w", name, ctxErr)
		}
		return out.buf.Bytes(), errOut.buf.Bytes(), truncated, 0, fmt.Errorf("run %s: %w", name, err)
	}
	return out.buf.Bytes(), errOut.buf.Bytes(), truncated, 0, nil
}

// limitedWriter keeps the first MaxCaptureBytes and drops the rest, so a
// command dumping gigabytes cannot exhaust memory (protocol §4.1 cap).
type limitedWriter struct {
	buf       bytes.Buffer
	truncated bool
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	room := sandbox.MaxCaptureBytes - w.buf.Len()
	switch {
	case room <= 0:
		w.truncated = true
	case len(p) > room:
		w.buf.Write(p[:room])
		w.truncated = true
	default:
		w.buf.Write(p)
	}
	return len(p), nil
}

// Probe exists to verify that the branch rule refuses a merge when a change
// adds a surviving mutant, and nothing asserts it on purpose. It ships on a
// throwaway branch and is deleted with it — if this reaches main, the
// mutation gate did not do the one thing it was built for.
func Probe(n int) bool {
	return n > 0
}
