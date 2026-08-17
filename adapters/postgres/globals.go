package main

import (
	"context"
	"fmt"
	"strings"
)

// globals.go loads the cluster-level objects a per-database dump cannot
// carry: roles, their memberships, and the grants that reference them.
//
// A logical recovery runs globals first, then the database dumps. A drill
// that skips the first half proves less than the recovery path it stands
// for — and fails loudly the moment a restored GRANT names a role that was
// never created, which is what `pg_restore --no-owner` leaves behind
// (--no-owner drops OWNER TO, never GRANT).
//
// Two properties of a real pg_dumpall --globals-only script drive the
// implementation below, and both were measured against postgres:16 rather
// than assumed.

// bootstrapRoleExists is the one failure every globals script produces
// against a fresh cluster: pg_dumpall emits CREATE ROLE for the bootstrap
// superuser too, and initdb already created it. Tolerating exactly this
// one error — and nothing else — is what keeps the load honest.
//
// The obvious alternative, ON_ERROR_STOP=1, is worse than it looks: the
// collision lands in the middle of the script, so psql would stop there
// and silently skip every role that sorts after the superuser's name. The
// completeness of the load would depend on role naming.
func bootstrapRoleExists(user string) string {
	return fmt.Sprintf("role %q already exists", user)
}

// loadGlobals transfers the globals script into the sandbox and replays it
// with psql, returning the transfer and load durations separately so the
// caller can account for them in the right phases.
//
// The script is replayed by psql reading it whole, never through the
// sql_runner: pg_dumpall wraps its output in \restrict/\unrestrict
// meta-commands, which only a psql session reading a script can execute.
//
// A globals script stored gzip-compressed is replayed as stored, through
// the same scripts the dump uses (see psqlReplayArgv). The two members are
// sniffed independently because a backup job is free to compress one and
// not the other.
func loadGlobals(ctx context.Context, c *core, user, hostPath string, globals sandboxFile) (transfer, load float64, perr *protoError) {
	put, perr := c.putFile(ctx, putFileArgs{SourcePath: hostPath, DestPath: globals.path, Mode: "0600"})
	if perr != nil {
		return 0, 0, perr
	}

	// Cluster-level objects are not owned by any database; connect to the
	// maintenance database initdb always creates, which exists whatever the
	// drill restores into.
	//
	// ON_ERROR_STOP stays off deliberately (see bootstrapRoleExists), which
	// also means psql's exit code says nothing about the load: the verdict
	// comes from classifying its diagnostics. --echo-errors is deliberately
	// absent — it would echo the failing statement, and a globals script's
	// statements carry role password hashes.
	val, _, stderr, perr := c.exec(ctx, execArgs{
		// The empty fence: a pg_dumpall --globals-only script carries roles
		// and tablespaces, never extensions, so there is nothing to fence.
		Argv: psqlReplayArgv(globals, user, defaultDatabase, errorStopOff, ""),
	})
	if perr != nil {
		return 0, 0, perr
	}
	if failure := globalsFailure(stderr, user); failure != "" {
		return 0, 0, protoErr("restore_failed", false, "loading cluster globals failed: %s", failure)
	}
	// A truncated globals script is the one failure psql cannot report here
	// by construction: ON_ERROR_STOP is off, so it replays what it was given
	// and exits content, having created however many roles survived. The
	// replay script checks the script's own closing line for exactly that
	// reason — a drill that restored half the cluster's roles must not pass.
	if perr := mapScriptExit(val.ExitCode, stderr, "the cluster globals script"); perr != nil {
		return 0, 0, perr
	}
	if val.ExitCode != 0 {
		// No classified error, yet psql still refused: the client itself
		// failed (unreadable script, lost connection).
		return 0, 0, protoErr("restore_failed", false,
			"psql exited %d loading cluster globals: %s", val.ExitCode, firstLine(stderr))
	}
	return put.DurationSeconds, val.DurationSeconds, nil
}

// errorMarkers are the prefixes psql uses for a failure. NOTICE, WARNING,
// and the DETAIL/HINT continuation lines are not failures and are ignored:
// a globals script routinely produces them.
var errorMarkers = []string{"ERROR:", "FATAL:", "PANIC:", "psql: error:"}

// globalsFailure returns the first diagnostic line that is not the
// tolerated bootstrap-role collision, or "" when the load is acceptable.
// The returned line is safe to embed in a protocol message.
func globalsFailure(stderr []byte, user string) string {
	tolerated := bootstrapRoleExists(user)
	for _, line := range strings.Split(string(stderr), "\n") {
		message, isFailure := diagnosticMessage(line)
		if !isFailure || message == tolerated {
			continue
		}
		return firstLine([]byte(line))
	}
	return ""
}

// diagnosticMessage reports whether a psql stderr line is a failure and, if
// so, returns the server message with the psql locator and severity prefix
// removed.
func diagnosticMessage(line string) (string, bool) {
	for _, marker := range errorMarkers {
		if i := strings.Index(line, marker); i >= 0 {
			return strings.TrimSpace(line[i+len(marker):]), true
		}
	}
	return "", false
}
