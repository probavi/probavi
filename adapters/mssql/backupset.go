package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// backupset.go decides which backup a drill restores, and from which set
// inside it.
//
// A SQL Server backup directory holds full, differential, and transaction
// log backups side by side, and the newest file is typically a log backup —
// which cannot create a database. Picking by mtime alone therefore reports
// restore_failed on a perfectly restorable backup set: a false alarm, and
// the direction that costs an operator's trust rather than merely
// withholding it.
//
// Nothing in the file name or the bytes says which type a file is: the
// extensions are convention (SQL Server ignores them), and full,
// differential, and log backups share one media format. Only the engine
// knows, through RESTORE HEADERONLY — so the choice happens inside the
// sandbox, after the engine is up, and the host contributes only what it
// can answer honestly: which files are backup media at all.

// Documented BackupType values from RESTORE HEADERONLY. Only a full
// database backup can create the drill's database; everything else is a
// valid backup that needs one first.
const (
	backupTypeFull         = 1
	backupTypeLog          = 2
	backupTypeFile         = 4
	backupTypeDifferential = 5
	backupTypeDiffFile     = 6
	backupTypePartial      = 7
	backupTypeDiffPartial  = 8
)

// headerColumns is the smallest prefix of the RESTORE HEADERONLY result
// this adapter reads: BackupName, BackupDescription, BackupType,
// ExpirationDate, Compressed, Position. The two it uses are at fixed,
// stable positions at the very front of a result set that has grown by
// appending columns for decades.
const (
	headerColumnCount = 6
	headerTypeIndex   = 2
	headerPositionIdx = 5
)

// backupMediaMagic are the first four bytes of SQL Server backup media,
// measured on a real server: "TAPE" for an ordinary backup and "MSSQ" for
// a compressed one. This is a *skip* filter for directory scanning only —
// it keeps checksum sidecars and log files from being transferred and
// probed, and it is never consulted for a file the drill config names
// outright. Because the list is measured rather than promised by
// Microsoft, a file it skips is named in the failure message, so an
// operator can see exactly what was passed over.
var backupMediaMagic = []string{"TAPE", "MSSQ"}

// looksLikeBackupMedia reports whether a file starts like SQL Server
// backup media. An unreadable file is not a candidate; the drill fails
// later with a precise message if nothing is.
func looksLikeBackupMedia(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	head := make([]byte, 4)
	n, readErr := f.Read(head)
	if err := f.Close(); err != nil || readErr != nil || n < 4 {
		return false
	}
	for _, magic := range backupMediaMagic {
		if string(head) == magic {
			return true
		}
	}
	return false
}

// backupSet is one backup inside a media file. A single file may hold
// several: SQL Server appends to backup media unless told to overwrite.
type backupSet struct {
	position   int
	backupType int
}

// probeBackupSets asks the engine what a transferred file holds. The path
// is adapter-composed (the sandbox scratch destination), never operator
// input.
func probeBackupSets(ctx context.Context, c *core, sandboxPath string) ([]backupSet, *protoError) {
	val, stdout, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{sqlcmdPath, "-S", "127.0.0.1,1433", "-U", defaultUser,
			"-C", "-b", "-l", "5", "-h", "-1", "-W", "-s", "|", "-r", "1", "-Q",
			"SET NOCOUNT ON; RESTORE HEADERONLY FROM DISK = N'" + sandboxPath + "'"},
		Env: sqlcmdEnv(),
	})
	if perr != nil {
		return nil, perr
	}
	if val.ExitCode != 0 {
		return nil, mapRestoreFailure(stderr)
	}
	return parseBackupSets(stdout)
}

// parseBackupSets reads the pipe-separated HEADERONLY rows. Both fields it
// takes are integers, and a row whose columns do not line up is refused
// rather than guessed at: a backup name containing the separator would
// shift every later column, and silently reading the wrong one could pick
// a log backup while believing it is a full one.
func parseBackupSets(stdout []byte) ([]backupSet, *protoError) {
	lines := strings.Split(string(stdout), "\n")
	sets := make([]backupSet, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) < headerColumnCount {
			// Not a result row (sqlcmd notices, blank separators).
			continue
		}
		kind, err := strconv.Atoi(strings.TrimSpace(fields[headerTypeIndex]))
		if err != nil || !knownBackupType(kind) {
			return nil, protoErr("source_corrupt", false,
				"cannot read the backup header: unexpected column layout "+
					"(a backup name containing '|' does this) — point the drill at the backup file directly")
		}
		position, err := strconv.Atoi(strings.TrimSpace(fields[headerPositionIdx]))
		if err != nil || position < 1 {
			return nil, protoErr("source_corrupt", false,
				"cannot read the backup header: backup set position is not a number")
		}
		sets = append(sets, backupSet{position: position, backupType: kind})
	}
	// No rows at all is not a verdict: the engine answered without
	// classifying anything, which a real server does not do for media it
	// accepted — it exits non-zero instead. Callers fall back to the
	// engine's own set selection rather than inventing a claim, which also
	// keeps the adapter working under the protocol's simulated sandbox,
	// where every exec succeeds with a fixed stdout (§10).
	return sets, nil
}

func knownBackupType(t int) bool {
	switch t {
	case backupTypeFull, backupTypeLog, backupTypeFile, backupTypeDifferential,
		backupTypeDiffFile, backupTypePartial, backupTypeDiffPartial:
		return true
	}
	return false
}

// newestFullPosition returns the position of the newest full database
// backup in the media, if any. Sets are appended, so the highest position
// is the most recent — and restoring without naming one takes the *first*,
// which in an appended file is the oldest backup on it.
func newestFullPosition(sets []backupSet) (int, bool) {
	best := 0
	for _, s := range sets {
		if s.backupType == backupTypeFull && s.position > best {
			best = s.position
		}
	}
	return best, best > 0
}

// describeSets names what a media file holds, for a message that tells an
// operator what to do next instead of quoting the engine at them.
func describeSets(sets []backupSet) string {
	counts := map[string]int{}
	var order []string
	for _, s := range sets {
		name := backupTypeName(s.backupType)
		if counts[name] == 0 {
			order = append(order, name)
		}
		counts[name]++
	}
	parts := make([]string, 0, len(order))
	for _, name := range order {
		if counts[name] == 1 {
			parts = append(parts, name)
			continue
		}
		parts = append(parts, fmt.Sprintf("%d %ss", counts[name], name))
	}
	return strings.Join(parts, ", ")
}

func backupTypeName(t int) string {
	switch t {
	case backupTypeFull:
		return "full backup"
	case backupTypeLog:
		return "transaction log backup"
	case backupTypeDifferential, backupTypeDiffFile, backupTypeDiffPartial:
		return "differential backup"
	case backupTypeFile:
		return "file backup"
	case backupTypePartial:
		return "partial backup"
	default:
		return "backup"
	}
}

// defaultBackupSet is the set the engine restores when none is named: the
// first one on the media. It is what this adapter falls back to when the
// header says nothing at all, so an unclassifiable answer changes nothing
// rather than inventing a claim about the media.
const defaultBackupSet = 1

// selection is the outcome of choosing what to restore.
type selection struct {
	hostPath string  // the artifact whose bytes become the backup identity
	position int     // the backup set inside it (RESTORE ... WITH FILE = n)
	transfer float64 // seconds spent transferring the chosen artifact
}

// selectBackup transfers candidates into the sandbox newest-first and asks
// the engine what each one is, stopping at the first that can create a
// database. Rejected candidates are simply overwritten by the next
// transfer — they share one destination path, so nothing accumulates.
//
// Only the chosen artifact's transfer is reported: probing is how the
// drill finds the backup, not part of the recovery it measures, and an
// operator recovering for real reads their backup catalogue instead.
func selectBackup(ctx context.Context, c *core, plan *sourcePlan, destPath string) (*selection, *protoError) {
	if plan.fixed != "" {
		return selectNamed(ctx, c, plan.fixed, destPath)
	}
	rejected := make([]string, 0, len(plan.candidates))
	for _, candidate := range plan.candidates {
		put, perr := c.putFile(ctx, putFileArgs{SourcePath: candidate, DestPath: destPath, Mode: "0600"})
		if perr != nil {
			return nil, perr
		}
		sets, perr := probeBackupSets(ctx, c, destPath)
		if perr != nil {
			// The file is backup media the engine cannot read. Falling back
			// to an older backup here would hide exactly what a drill exists
			// to surface, so this fails the drill.
			return nil, perr
		}
		if len(sets) == 0 {
			// Nothing to go on: take this candidate the way the pre-header
			// rule would have, rather than skipping media the engine did
			// not refuse.
			return &selection{hostPath: candidate, position: defaultBackupSet, transfer: put.DurationSeconds}, nil
		}
		if position, ok := newestFullPosition(sets); ok {
			return &selection{hostPath: candidate, position: position, transfer: put.DurationSeconds}, nil
		}
		rejected = append(rejected, fmt.Sprintf("%s: %s", filepath.Base(candidate), describeSets(sets)))
	}
	return nil, noFullBackup(plan, rejected)
}

// selectNamed handles an artifact the drill config names outright: it is
// transferred and probed like any other, but never skipped — the operator
// asked for this file, so the answer is about this file.
func selectNamed(ctx context.Context, c *core, hostPath, destPath string) (*selection, *protoError) {
	put, perr := c.putFile(ctx, putFileArgs{SourcePath: hostPath, DestPath: destPath, Mode: "0600"})
	if perr != nil {
		return nil, perr
	}
	sets, perr := probeBackupSets(ctx, c, destPath)
	if perr != nil {
		return nil, perr
	}
	if len(sets) == 0 {
		return &selection{hostPath: hostPath, position: defaultBackupSet, transfer: put.DurationSeconds}, nil
	}
	position, ok := newestFullPosition(sets)
	if !ok {
		return nil, protoErr("invalid_request", false,
			"%s holds no full backup (%s): a differential or log backup cannot create a database — "+
				"point the drill at the full backup it builds on",
			filepath.Base(hostPath), describeSets(sets))
	}
	return &selection{hostPath: hostPath, position: position, transfer: put.DurationSeconds}, nil
}

// noFullBackup explains an exhausted directory scan: what was examined and
// what it was, plus anything skipped for not looking like backup media, so
// the operator can see the whole basis of the verdict.
func noFullBackup(plan *sourcePlan, rejected []string) *protoError {
	detail := "no files to examine"
	if len(rejected) > 0 {
		detail = "examined " + nameList(rejected, 5)
	}
	if len(plan.skipped) > 0 {
		detail += "; skipped " + nameList(plan.skipped, 5) + " (not backup media)"
	}
	return protoErr("source_not_found", false,
		"backup directory %s holds no full backup: %s", plan.dir, detail)
}

// nameList joins names for a protocol message, capped so one crowded
// directory cannot inflate the error field.
func nameList(names []string, limit int) string {
	if len(names) <= limit {
		return firstLine([]byte(strings.Join(names, "; ")))
	}
	head := strings.Join(names[:limit], "; ")
	return firstLine([]byte(head)) + " and " + strconv.Itoa(len(names)-limit) + " more"
}
