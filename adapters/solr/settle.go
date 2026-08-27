package main

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// settle.go refuses a backup a job is still writing.
//
// When a drill names a directory, the adapter picks the artifact itself —
// and the newest file in a backup directory is quite often the one being
// written right now. Restoring it reads a truncated artifact, which every
// engine rejects, so the drill reports a failure against a backup set that
// is perfectly healthy.
//
// The check is deliberately not a filter. Skipping an unsettled artifact
// and restoring the previous one would be worse than the false alarm it
// avoids: the drill would prove an older backup while the record implied
// the newest, and nothing in the evidence would say so. So an artifact
// still in motion fails the drill with a message that says what to do.
//
// A backup job that writes to a temporary name and renames on completion
// never trips this at all — the directory only ever shows finished files.
// That is the real fix, and the adapter README recommends it.

// settleWindow is how long an artifact must have been still before a drill
// will restore it. It is a guard against catching a writer mid-file, not a
// guarantee: a writer stalled longer than this (slow network storage) can
// still look finished, which is why the rename pattern is what actually
// removes the race.
const settleWindow = 750 * time.Millisecond

// fileState is one observation of an artifact.
type fileState struct {
	size  int64
	mtime time.Time
}

// settled reports whether two observations of the same artifact describe a
// file nothing is writing to.
func settled(before, after fileState) bool {
	return before.size == after.size && before.mtime.Equal(after.mtime)
}

// observe reads an artifact's size and modification time.
// observe measures an artifact the way this engine ships it.
//
// A Solr backup is a directory, and a directory's own size and mtime say
// nothing about a file growing inside it — an in-flight backup would look
// perfectly still. So a directory is measured as its tree: every regular
// file's size summed, and the newest mtime anywhere under it.
func observe(path string) (fileState, *protoError) {
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return fileState{}, protoErr("source_not_found", false, "backup source does not exist: %s", path)
	case err != nil:
		return fileState{}, protoErr("source_unreadable", false, "stat backup source: %v", err)
	case !info.IsDir():
		return fileState{size: info.Size(), mtime: info.ModTime()}, nil
	}
	state := fileState{mtime: info.ModTime()}
	err = filepath.WalkDir(path, func(_ string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		fi, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		if d.Type().IsRegular() {
			state.size += fi.Size()
		}
		if fi.ModTime().After(state.mtime) {
			state.mtime = fi.ModTime()
		}
		return nil
	})
	if err != nil {
		return fileState{}, protoErr("source_unreadable", false, "read backup source: %v", err)
	}
	return state, nil
}

// assertSettled refuses an artifact that changed while being looked at.
// An artifact untouched for longer than the window is taken as finished
// without waiting, so an ordinary drill against last night's backup pays
// nothing for this check.
func assertSettled(ctx context.Context, path string, window time.Duration) *protoError {
	before, perr := observe(path)
	if perr != nil {
		return perr
	}
	if time.Since(before.mtime) >= window {
		return nil
	}
	select {
	case <-ctx.Done():
		return protoErr("cancelled", true, "cancelled while checking whether the backup is complete")
	case <-time.After(window):
	}
	after, perr := observe(path)
	if perr != nil {
		return perr
	}
	if settled(before, after) {
		return nil
	}
	return protoErr("source_unreadable", false,
		"backup %s is still being written: run the drill after the backup job finishes, or have the job "+
			"write to a temporary name and rename it on completion, so a drill never sees a partial file",
		filepath.Base(path))
}
