package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"time"
)

// resolvedSource is a concrete backup artifact chosen for restore.
type resolvedSource struct {
	path      string
	checksum  string // "sha256:<hex>" over the artifact bytes
	sizeBytes int64
}

// resolveSource maps a source kind to one restorable artifact.
//
//	oracle_datapump — path is one Data Pump dump file (expdp output)
//
// The host vets nothing about the bytes: the dump file format is
// Oracle's own and undocumented, so every verdict about what the file is
// comes from the engine inside the sandbox, through the documented
// header reader (header.go). Metadata is a bonus, verdicts are not
// guesses.
func resolveSource(kind, path string) (*resolvedSource, *protoError) {
	if kind != "oracle_datapump" {
		return nil, protoErr("unsupported_source", false,
			"unsupported source kind: %s (supported: oracle_datapump)", kind)
	}
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup source does not exist: %s", path)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup source: %v", err)
	case info.IsDir():
		return nil, protoErr("invalid_request", false,
			"source path %s is a directory; oracle_datapump names one dump file", path)
	case info.Size() == 0:
		return nil, protoErr("source_corrupt", false, "backup source %s is empty", path)
	}
	checksum, perr := fileChecksum(path)
	if perr != nil {
		return nil, perr
	}
	return &resolvedSource{path: path, checksum: checksum, sizeBytes: info.Size()}, nil
}

// fileChecksum streams the artifact once. The hash feeds the evidence
// record's backup identity, so it must be a real measurement of the bytes
// that will be restored.
func fileChecksum(path string) (string, *protoError) {
	f, err := os.Open(path)
	if err != nil {
		return "", protoErr("source_unreadable", false, "read backup source: %v", err)
	}
	h := sha256.New()
	_, cerr := io.Copy(h, f)
	if err := f.Close(); err != nil && cerr == nil {
		cerr = err
	}
	if cerr != nil {
		return "", protoErr("source_unreadable", false, "read backup source: %v", cerr)
	}
	return fmt.Sprintf("sha256:%s", hex.EncodeToString(h.Sum(nil))), nil
}

// backupTimezoneParam names the IANA zone the backup host was in.
//
// backup.created_at in an evidence record is an absolute instant, and a
// Data Pump dump does not record one: its header carries the export's
// wall clock as the source instance printed it — `Fri Aug 21 12:04:02
// 2026`, no offset (measured through the engine's own header reader).
// The offset is a fact only the operator has, so the drill config
// supplies it — by name rather than as a number, because the offset
// depends on the date of the backup. Nothing is guessed: without the
// declaration the adapter reports no creation time, and the record's
// created_at is null, which the evidence schema provides for precisely
// because a backup's own creation time is not always derivable.
const backupTimezoneParam = "backup_timezone"

// createdAtLayout keeps the offset in the value; the core normalizes it to
// UTC when it writes the record (adapter protocol §6.2).
const createdAtLayout = "2006-01-02T15:04:05.000Z07:00"

// headerClockLayout is the form the header reader prints the export
// instant in: C's asctime, the day space-padded (measured).
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
