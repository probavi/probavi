package main

import (
	"context"
	"strconv"
	"strings"
)

// engineversion.go compares the backup's origin server against the engine
// it is about to be restored into.
//
// SQL Server's version rule is asymmetric, unlike the sibling physical
// engines: a backup restores onto an engine of the same or a newer major
// (that is the supported upgrade path), but never onto an older one. So
// the pre-check (docs/engine-versions.md §5) refuses only the downgrade —
// the direction the engine itself would refuse a few minutes later with
// an error naming internal version numbers, after the restore had already
// been attempted. The backup states its origin in the header the adapter
// already reads (RESTORE HEADERONLY, SoftwareVersionMajor); the running
// engine answers SERVERPROPERTY. Both sides refuse only on positive
// evidence: an absent or implausible value skips the check, and the
// restore then speaks for itself.

// sqlServerMajorNames maps the major versions this repository's matrix
// has met to their product names, purely for readable messages; an
// unmapped major is shown as the bare number.
var sqlServerMajorNames = map[int]string{
	13: "SQL Server 2016",
	14: "SQL Server 2017",
	15: "SQL Server 2019",
	16: "SQL Server 2022",
	17: "SQL Server 2025",
}

// plausibleSQLServerMajor parses a header or SERVERPROPERTY value into a
// major version, 0 when it is not one. The bounds reject what a shifted
// header row would put there — a vendor id (4608), a compatibility level
// (160) — so a text column containing the separator cannot turn into a
// refusal.
func plausibleSQLServerMajor(value string) int {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || n < 9 || n > 40 {
		return 0
	}
	return n
}

// describeSQLServerMajor renders a major version for a message.
func describeSQLServerMajor(major int) string {
	if name, ok := sqlServerMajorNames[major]; ok {
		return name
	}
	return "SQL Server major version " + strconv.Itoa(major)
}

// engineMajorVersion asks the running engine for its major version, 0
// when the answer is missing or implausible. Only a sandbox verb failure
// is an error — an unqueryable version is not this check's refusal to
// make.
func engineMajorVersion(ctx context.Context, c *core) (int, *protoError) {
	val, stdout, _, perr := c.exec(ctx, execArgs{
		Argv: []string{sqlcmdPath, "-S", "127.0.0.1,1433", "-U", defaultUser,
			"-C", "-b", "-l", "5", "-h", "-1", "-W", "-r", "1", "-Q",
			"SET NOCOUNT ON; SELECT CONVERT(int, SERVERPROPERTY('ProductMajorVersion'))"},
		Env: sqlcmdEnv(),
	})
	if perr != nil {
		return 0, perr
	}
	if val.ExitCode != 0 {
		return 0, nil
	}
	return plausibleSQLServerMajor(string(stdout)), nil
}

// checkEngineVersion refuses the restore direction SQL Server does not
// have: a backup from a newer major handed to an older engine. Same or
// older backups pass — restoring them upward is the supported path.
func checkEngineVersion(ctx context.Context, c *core, backupMajor int) *protoError {
	if backupMajor == 0 {
		return nil
	}
	engineMajor, perr := engineMajorVersion(ctx, c)
	if perr != nil {
		return perr
	}
	if engineMajor == 0 || backupMajor <= engineMajor {
		return nil
	}
	return protoErr("invalid_request", false,
		"the backup was taken by %s, but the sandbox engine is %s: SQL Server never restores a backup onto an older engine — use a sandbox image at least as new as the backup's server",
		describeSQLServerMajor(backupMajor), describeSQLServerMajor(engineMajor))
}
