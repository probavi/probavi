package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// resolvedSource is a concrete backup artifact chosen for restore.
type resolvedSource struct {
	path      string
	checksum  string // "sha256:<hex>" over the archive's bytes
	sizeBytes int64
	// createdAt is when the engine says the backup began, read from the
	// artifact's own header and anchored with the declared zone. It is the
	// engine's record rather than a file's mtime, which would date a copy.
	createdAt *string
}

// resolveSource maps a source kind to one restorable artifact.
//
//	firebird_gbak     — path is one gbak transportable backup file
//	firebird_gbak_dir — path is a directory of them; source.params.select
//	                    picks one, newest by default (selection.go)
func resolveSource(ctx context.Context, kind, path string, params map[string]string) (*resolvedSource, *protoError) {
	loc, perr := backupLocation(params)
	if perr != nil {
		return nil, perr
	}
	policy, perr := backupSelection(kind, params)
	if perr != nil {
		return nil, perr
	}
	switch kind {
	case "firebird_gbak":
		return resolveBackup(path, loc)
	case "firebird_gbak_dir":
		chosen, perr := chooseBackupIn(ctx, path, policy)
		if perr != nil {
			return nil, perr
		}
		return resolveBackup(chosen, loc)
	default:
		return nil, protoErr("unsupported_source", false,
			"unsupported source kind: %s (supported: firebird_gbak, firebird_gbak_dir)", kind)
	}
}

func resolveBackup(path string, loc *time.Location) (*resolvedSource, *protoError) {
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup source does not exist: %s", path)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup source: %v", err)
	case info.IsDir():
		return nil, protoErr("invalid_request", false,
			"source path %s is a directory; use kind firebird_gbak_dir for a directory of backups", path)
	}
	sum, perr := fileChecksum(path)
	if perr != nil {
		return nil, perr
	}
	// The header is read for the clock only. It is not a verdict on the
	// artifact: gbak inside the sandbox is what accepts or rejects a
	// backup, and a host-side guess at validity would be a second opinion
	// nobody asked for. A header that does not parse simply dates nothing.
	clock, cerr := readBackupClock(path)
	if cerr != nil {
		// Not a verdict, deliberately: an artifact whose header this
		// cannot read simply dates nothing, and gbak inside the sandbox
		// is what decides whether it restores.
		clock = ""
	}
	return &resolvedSource{
		path: path, checksum: sum, sizeBytes: info.Size(),
		createdAt: createdAt(clock, loc),
	}, nil
}

// fileChecksum streams the archive once. A gbak backup is a single file,
// so the checksum in the evidence record is a hash of exactly the bytes
// that will be restored — no canonical ordering rule needed.
func fileChecksum(path string) (string, *protoError) {
	f, err := os.Open(path) //#nosec G304 -- the artifact the drill named.
	if err != nil {
		return "", protoErr("source_unreadable", false, "open %s: %v", path, err)
	}
	h := sha256.New()
	_, cerr := io.Copy(h, f)
	if err := f.Close(); err != nil && cerr == nil {
		cerr = err
	}
	if cerr != nil {
		return "", protoErr("source_unreadable", false, "read %s: %v", path, cerr)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// chooseBackupIn picks the backup the policy asks for, ordered by file
// time (selection.go says why that is all there is).
func chooseBackupIn(ctx context.Context, dir string, policy selectPolicy) (string, *protoError) {
	entries, err := os.ReadDir(dir)
	switch {
	case os.IsNotExist(err):
		return "", protoErr("source_not_found", false, "backup directory does not exist: %s", dir)
	case err != nil:
		return "", protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	candidates := make([]dirCandidate, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			return "", protoErr("source_unreadable", false, "stat %s: %v", e.Name(), ierr)
		}
		candidates = append(candidates, dirCandidate{
			path: filepath.Join(dir, e.Name()), name: e.Name(), mtime: info.ModTime(),
		})
	}
	if len(candidates) == 0 {
		return "", protoErr("source_not_found", false, "backup directory %s contains no backups", dir)
	}
	chosen := pick(candidates, policy).path
	// The adapter chose this backup, not the operator — under every policy:
	// make sure a backup job is not still writing it (see settle.go).
	if perr := assertSettled(ctx, chosen, settleWindow); perr != nil {
		return "", perr
	}
	return chosen, nil
}

// backupTimezoneParam names the IANA zone the backup host was in.
//
// backup.created_at in an evidence record is an absolute instant, and a
// gbak backup does not record one: its header carries the wall clock of
// the host that took it, in C's asctime form and with no offset —
// `Fri Aug 28 16:48:55 2026`, measured on Firebird 4.0.7 and 5.0.4 alike.
// The offset is a fact only the operator has, so the drill config supplies
// it — by name rather than as a number, because the offset depends on the
// date of the backup. Nothing is guessed: without the declaration the
// adapter reports no creation time, and the record's created_at is null,
// which the evidence schema provides for precisely because a backup's own
// creation time is not always derivable.
const backupTimezoneParam = "backup_timezone"

// createdAtLayout keeps the offset in the value; the core normalizes it to
// UTC when it writes the record (adapter protocol §6.2).
const createdAtLayout = "2006-01-02T15:04:05.000Z07:00"

// headerClockLayout is the form gbak writes the backup instant in: C's
// asctime, the day space-padded (measured).
const headerClockLayout = "Mon Jan _2 15:04:05 2006"

// backupLocation resolves the declared zone, or nil when none is declared.
// An unknown name fails the drill immediately rather than silently
// dropping the timestamp it was supposed to make exact.
func backupLocation(params map[string]string) (*time.Location, *protoError) {
	name := params[backupTimezoneParam]
	if name == "" {
		return nil, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, protoErr("invalid_request", false,
			"source.params.%s must be an IANA time zone name such as Europe/Budapest or UTC: %s is not one",
			backupTimezoneParam, name)
	}
	return loc, nil
}

// createdAt turns the header's wall clock into an instant in the declared
// zone; nil when no zone is declared or the clock did not parse.
func createdAt(clock string, loc *time.Location) *string {
	if loc == nil || clock == "" {
		return nil
	}
	t, err := time.ParseInLocation(headerClockLayout, clock, loc)
	if err != nil {
		return nil
	}
	s := t.Format(createdAtLayout)
	return &s
}

// The gbak header is a flat sequence of <tag><length><data> records after
// a two-byte format marker. Measured on Firebird 4.0.7 and 5.0.4: both
// write the identical structure, tag 0x07 carrying the source database
// path and tag 0x01 carrying the backup clock as 24 asctime bytes.
//
// The same measurement is why this adapter cannot pre-check engine
// versions the way docs/engine-versions.md §5 asks of physical restores:
// nothing in the header names the engine that wrote it.
const (
	headerMagic     = 0x0002
	headerClockTag  = 0x01
	headerClockLen  = 24
	headerScanBytes = 4096
)

var errNoClock = errors.New("no backup clock in the header")

// readBackupClock returns the asctime string gbak stamped into the
// artifact. Only the head of the file is walked: the records this reads
// sit in the first hundred bytes, and a scan that ran to EOF would turn
// an unreadable multi-gigabyte archive into a long wait for nothing.
func readBackupClock(path string) (string, error) {
	f, err := os.Open(path) //#nosec G304 -- the artifact the drill named.
	if err != nil {
		return "", err
	}
	head := make([]byte, headerScanBytes)
	n, rerr := io.ReadFull(f, head)
	if rerr != nil && !errors.Is(rerr, io.ErrUnexpectedEOF) && !errors.Is(rerr, io.EOF) {
		rerr = fmt.Errorf("read %s: %w", path, rerr)
	} else {
		rerr = nil
	}
	if cerr := f.Close(); cerr != nil && rerr == nil {
		rerr = cerr
	}
	if rerr != nil {
		return "", rerr
	}
	head = head[:n]
	if len(head) < 2 || int(head[0])<<8|int(head[1]) != headerMagic {
		return "", fmt.Errorf("not a gbak backup: %w", errNoClock)
	}
	for i := 2; i+2 <= len(head); {
		tag, size := head[i], int(head[i+1])
		if i+2+size > len(head) {
			break
		}
		data := head[i+2 : i+2+size]
		// Tag 0x01 is reused for shorter records; the clock is the one
		// carrying an asctime of the documented width that actually parses.
		if tag == headerClockTag && size == headerClockLen {
			if _, perr := time.Parse(headerClockLayout, string(data)); perr == nil {
				return string(data), nil
			}
		}
		i += 2 + size
	}
	return "", errNoClock
}
