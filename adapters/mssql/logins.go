package main

import (
	"context"
	"regexp"

	"strings"
)

// logins.go replays the server-level principals a database backup cannot
// carry: SQL Server logins live in master, database users live inside the
// .bak, linked by SID. A restore without the logins brings back the users
// orphaned — RESTORE succeeds, checks run, and the application principal
// still cannot log in. The bak_with_logins kind replays a logins script
// before the restore and then refuses to pass while any restored SQL user
// lacks a matching server login, so the record's claim covers the whole
// principal chain.
//
// The behavior below was measured against a real SQL Server 2022 instance
// rather than assumed; each constant notes what the measurement showed.

// loginsScriptArgv replays a T-SQL script file. Deliberately without -b:
// with it, sqlcmd stops at the first failed batch and silently skips every
// login after it — the completeness of the replay would depend on login
// ordering (measured: a mid-script collision under -b left the following
// CREATE LOGIN unexecuted). Without -b a failed statement stops neither
// its batch nor the script, the exit code stays 0, and the verdict comes
// from classifying stderr instead. -r 0 keeps that stream clean: only
// severity >= 11 diagnostics land there, while PRINT and informational
// output stay on stdout.
func loginsScriptArgv(path string) []string {
	return []string{sqlcmdPath, "-S", "127.0.0.1,1433", "-U", defaultUser,
		"-C", "-l", "5", "-r", "0", "-i", path}
}

// loadLogins transfers the logins script into the sandbox and replays it,
// returning the transfer and load durations separately so the caller can
// account for them in the right phases.
func loadLogins(ctx context.Context, c *core, hostPath, sandboxPath string) (transfer, load float64, perr *protoError) {
	put, perr := c.putFile(ctx, putFileArgs{SourcePath: hostPath, DestPath: sandboxPath, Mode: "0600"})
	if perr != nil {
		return 0, 0, perr
	}
	val, _, stderr, perr := c.exec(ctx, execArgs{
		Argv: loginsScriptArgv(sandboxPath),
		Env:  sqlcmdEnv(),
	})
	if perr != nil {
		return 0, 0, perr
	}
	if failure := loginsFailure(stderr); failure != "" {
		return 0, 0, protoErr("restore_failed", false, "loading server logins failed: %s", failure)
	}
	if val.ExitCode != 0 {
		// No classified diagnostic, yet sqlcmd still refused: the client
		// itself failed (unreadable script, lost connection).
		return 0, 0, protoErr("restore_failed", false,
			"sqlcmd exited %d loading server logins: %s", val.ExitCode, firstLine(stderr))
	}
	return put.DurationSeconds, val.DurationSeconds, nil
}

// msgHeader is the engine diagnostic header sqlcmd emits before each
// message text line ("Msg 15025, Level 16, State 1, Server x, Line 1").
var msgHeader = regexp.MustCompile(`^Msg (\d+), Level \d+`)

// principalExists extracts the principal name from the one tolerable
// failure's message text.
var principalExists = regexp.MustCompile(`^The server principal '(.+)' already exists\.$`)

// loginsFailure returns the first stderr diagnostic that is not a
// tolerated collision, or "" when the replay is acceptable. The returned
// line is safe to embed in a protocol message.
//
// Exactly one failure class is tolerated — Msg 15025 for a principal the
// sandbox engine itself created — and nothing else, which is what keeps
// the replay honest. Two kinds of principals pre-exist in every fresh
// sandbox (measured on a stock instance): the sa login the adapter
// operates as, and the ##...##-wrapped internal principals SQL Server
// setup creates (both ##MS_Policy...## logins appear in sys.sql_logins,
// so faithful exports carry them). Collisions on those prove nothing
// missing; a collision on any other name is a genuine script defect and
// fails the drill.
func loginsFailure(stderr []byte) string {
	pendingMsg := ""
	for _, line := range strings.Split(string(stderr), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if m := msgHeader.FindStringSubmatch(line); m != nil {
			pendingMsg = m[1]
			continue
		}
		if pendingMsg == "" {
			// Text with no engine header is the client speaking
			// ("Sqlcmd: Error: ...", "Sqlcmd: '...': Invalid filename.")
			// — never tolerable.
			return firstLine([]byte(line))
		}
		if pendingMsg == "15025" {
			if m := principalExists.FindStringSubmatch(line); m != nil && bootstrapPrincipal(m[1]) {
				pendingMsg = ""
				continue
			}
		}
		return firstLine([]byte(line))
	}
	return ""
}

// bootstrapPrincipal reports whether a principal name belongs to the
// sandbox engine itself rather than to restored content: the connecting
// superuser, or the ##...## name space reserved for internal principals.
func bootstrapPrincipal(name string) bool {
	if name == defaultUser {
		return true
	}
	return strings.HasPrefix(name, "##") && strings.HasSuffix(name, "##")
}

// orphanQuery lists restored SQL-authentication users with no matching
// server login. Deliberately a single bare column: concatenating catalog
// columns raises a collation conflict (Msg 451) when the restored
// database's collation differs from the instance's (measured), and the
// separator-free one-name-per-line output needs no parsing beyond
// splitting. Scope: SQL users mapped to instance logins (type S,
// authentication_type 1). Windows principals cannot be recreated in a
// Linux sandbox and contained or WITHOUT LOGIN users need no login, so
// none of those can or should be proven here — the README states this.
const orphanQuery = "SET NOCOUNT ON; " +
	"SELECT dp.name FROM sys.database_principals dp " +
	"LEFT JOIN sys.server_principals sp ON dp.sid = sp.sid " +
	"WHERE dp.type = 'S' AND dp.authentication_type = 1 AND sp.sid IS NULL " +
	"ORDER BY dp.name"

// verifyLoginsCoverRestoredUsers is the gate that distinguishes this kind:
// after the restore it fails the provision while any restored SQL user is
// orphaned. Without it a bak_with_logins drill with an incomplete script
// would still pass — the same defect the kind exists to close, one level
// down.
func verifyLoginsCoverRestoredUsers(ctx context.Context, c *core, database string) *protoError {
	val, stdout, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{sqlcmdPath, "-S", "127.0.0.1,1433", "-U", defaultUser, "-d", database,
			"-C", "-b", "-l", "5", "-h", "-1", "-W", "-Q", orphanQuery},
		Env: sqlcmdEnv(),
	})
	if perr != nil {
		return perr
	}
	if val.ExitCode != 0 {
		// The kind cannot prove its claim, so the drill must not pass.
		return protoErr("restore_failed", false, "orphaned-user check failed: %s", verdictOrFirstLine(stderr))
	}
	if orphans := nonEmptyLines(stdout); len(orphans) > 0 {
		return protoErr("restore_failed", false,
			"restored database has users with no matching server login: %s", nameList(orphans, 5))
	}
	return nil
}

func nonEmptyLines(b []byte) []string {
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

func verdictOrFirstLine(stderr []byte) string {
	if v := verdictLine(stderr); v != "" {
		return v
	}
	return firstLine(stderr)
}
