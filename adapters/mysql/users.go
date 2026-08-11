package main

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// users.go replays the account layer a database dump cannot carry: MySQL
// accounts and grants live in the mysql system schema, never in a
// single-database dump. A restore without them succeeds silently while the
// application account cannot log in and every SQL SECURITY DEFINER object
// fails at invocation — the record would claim more than the drill proved.
// The mysqldump_with_users kind replays an exported accounts-and-grants
// script before the dump and then refuses to pass while the restored
// principal chain is broken.
//
// The behavior below was measured against a real MySQL 8.4 instance rather
// than assumed; each constant notes what the measurement showed.

// usersLoadScript replays the script through stdin with --force,
// deliberately: without --force the client aborts at the first failed
// statement and silently skips every account after it (measured), so the
// completeness of the replay would depend on account ordering — and the
// `source` client command aborts even WITH --force, so stdin is the only
// shape that continues. The exit code stays 0 under --force; the verdict
// comes from classifying stderr instead. The path and user travel as
// positional parameters, never interpolated into the script.
const usersLoadScript = `
mysql -h 127.0.0.1 -u "$1" -f < "$2"
rc=$?
[ "$rc" = 0 ] || exit "$rc"
[ -z "$3" ] || tail -c "$4" -- "$2" | grep -qE "$3" || exit 91
`

// compressedUsersLoadScript is the same replay fed by the decompressor.
// Its shape follows compressedRestoreScript, with one difference the
// --force above forces: the client does not abort, so its status is
// preserved rather than normalised, and only the decompressor's failure
// takes the reserved exit.
const compressedUsersLoadScript = `
rm -f "$2.fifo"
mkfifo "$2.fifo" || exit 92
tail -c "$5" <"$2.fifo" >"$2.tail" &
{ gzip -dc -- "$2"; echo $? > "$3"; } | tee "$2.fifo" | mysql -h 127.0.0.1 -u "$1" -f
loaded=$?
wait
[ "$(cat "$3")" = 0 ] || exit 90
[ "$loaded" = 0 ] || exit 1
[ -z "$4" ] || grep -qE "$4" "$2.tail" || exit 91
`

// loadUsers transfers the users script into the sandbox and replays it,
// returning the transfer and load durations separately so the caller can
// account for them in the right phases.
func loadUsers(ctx context.Context, c *core, user, hostPath string, users sqlMember, marker string) (transfer, load float64, perr *protoError) {
	put, perr := c.putFile(ctx, putFileArgs{SourcePath: hostPath, DestPath: users.path, Mode: "0600"})
	if perr != nil {
		return 0, 0, perr
	}
	argv := []string{"sh", "-c", usersLoadScript, "sh", user, users.path,
		marker, strconv.Itoa(markerTailBytes)}
	if users.compressed {
		argv = []string{"sh", "-c", compressedUsersLoadScript, "sh", user, users.path,
			users.statusPath(), marker, strconv.Itoa(markerTailBytes)}
	}
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: argv})
	if perr != nil {
		return 0, 0, perr
	}
	// The decompressor is judged before the replay: when it failed, every
	// diagnostic below it describes the consequence rather than the cause.
	// A truncated script is the failure this replay cannot report by
	// itself — --force means the client never aborts, so it creates the
	// accounts it got to and exits content (see complete.go).
	if perr := mapScriptExit(val.ExitCode, stderr, "the accounts script"); perr != nil {
		return 0, 0, perr
	}
	if failure := usersFailure(stderr); failure != "" {
		return 0, 0, protoErr("restore_failed", false, "loading user accounts failed: %s", failure)
	}
	if val.ExitCode != 0 {
		// No classified diagnostic, yet the client still refused: the load
		// itself failed (unreadable script, lost connection).
		return 0, 0, protoErr("restore_failed", false,
			"mysql exited %d loading user accounts: %s", val.ExitCode, firstLine(stderr))
	}
	return put.DurationSeconds, val.DurationSeconds, nil
}

// createUserFailed is the one tolerable failure's shape. The optional
// "at line N" and "in file: ..." fragments cover both client input modes.
var createUserFailed = regexp.MustCompile(
	`^ERROR 1396 \(HY000\)(?: at line \d+)?(?: in file: '[^']*')?: Operation CREATE USER failed for '([^']+)'@'[^']+'$`)

// usersFailure returns the first stderr diagnostic that is not a tolerated
// collision, or "" when the replay is acceptable. The returned line is
// safe to embed in a protocol message.
//
// Exactly one failure class is tolerated — ERROR 1396 (CREATE USER failed,
// which is how a collision with an existing account reports) for accounts
// the sandbox engine itself created — and nothing else. Two kinds of
// accounts pre-exist on every stock image (measured): root, and the
// reserved mysql.-prefixed system accounts (mysql.sys, mysql.session,
// mysql.infoschema), so faithful exports that include them collide.
// Collisions on those prove nothing missing; a collision on any other
// account is a genuine script defect and fails the drill. Exports written
// with CREATE USER IF NOT EXISTS produce warnings, not errors, and need no
// tolerance at all (measured).
func usersFailure(stderr []byte) string {
	for _, line := range strings.Split(string(stderr), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "ERROR") {
			// Non-ERROR lines are client warnings ("mysql: [Warning] ...");
			// a failure the client cares about always says ERROR.
			continue
		}
		if m := createUserFailed.FindStringSubmatch(line); m != nil && bootstrapAccount(m[1]) {
			continue
		}
		return firstLine([]byte(line))
	}
	return ""
}

// bootstrapAccount reports whether an account name belongs to the sandbox
// engine itself rather than to restored content: the image's root account,
// or the mysql.-prefixed name space MySQL reserves for system accounts.
func bootstrapAccount(user string) bool {
	return user == defaultUser || strings.HasPrefix(user, "mysql.")
}

// verifyPrincipalChain is the gate that distinguishes this kind: after the
// dump is loaded it fails the provision while the restored principal chain
// is broken. Without it a mysqldump_with_users drill with an incomplete or
// mismatched script would still pass — the same defect the kind exists to
// close, one level down.
//
// Three checks, each measured to catch a distinct real failure:
//
//  1. orphaned definers — objects whose DEFINER account does not exist
//     fail at invocation time (ERROR 1449) while restoring cleanly;
//  2. unresolvable views — a definer can exist yet lack rights (ERROR
//     1356), which happens precisely when the dump is restored under a
//     database name its grants do not cover; EXPLAIN surfaces it without
//     executing anything;
//  3. reachability — grants are database-scoped, so a script whose grants
//     name the production database leaves a differently-named restore
//     target unreachable by every restored account. This is the silent
//     variant of 2 when the database holds no definer objects at all.
//
// Reachability runs before the view check deliberately: when the database
// name is the problem, both fire, and the reachability message teaches the
// fix while the view error only names a symptom.
func verifyPrincipalChain(ctx context.Context, c *core, user, database string) *protoError {
	if perr := verifyDefinersExist(ctx, c, user, database); perr != nil {
		return perr
	}
	if perr := verifyDatabaseReachable(ctx, c, user, database); perr != nil {
		return perr
	}
	return verifyViewsResolve(ctx, c, user, database)
}

// runRows executes one SQL statement through the mysql client and returns
// the result rows. database is validated against databasePattern before it
// can be embedded in any of the statements below.
func runRows(ctx context.Context, c *core, user, sql string) ([]string, *protoError) {
	val, stdout, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"mysql", "-h", "127.0.0.1", "-u", user, "-N", "-B", "-e", sql},
	})
	if perr != nil {
		return nil, perr
	}
	if val.ExitCode != 0 {
		// The kind cannot prove its claim, so the drill must not pass.
		return nil, protoErr("restore_failed", false, "principal-chain check failed: %s", firstLine(stderr))
	}
	var rows []string
	for _, line := range strings.Split(string(stdout), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			rows = append(rows, line)
		}
	}
	return rows, nil
}

func verifyDefinersExist(ctx context.Context, c *core, user, database string) *protoError {
	query := fmt.Sprintf(
		"SELECT DISTINCT o.definer FROM ("+
			"SELECT definer, table_schema AS s FROM information_schema.views UNION ALL "+
			"SELECT definer, routine_schema FROM information_schema.routines UNION ALL "+
			"SELECT definer, trigger_schema FROM information_schema.triggers UNION ALL "+
			"SELECT definer, event_schema FROM information_schema.events) o "+
			"WHERE o.s = '%s' AND o.definer NOT IN "+
			"(SELECT CONCAT(user, '@', host) FROM mysql.user) ORDER BY o.definer",
		database)
	orphans, perr := runRows(ctx, c, user, query)
	if perr != nil {
		return perr
	}
	if len(orphans) > 0 {
		return protoErr("restore_failed", false,
			"restored objects name definers that do not exist: %s", nameList(orphans, 5))
	}
	return nil
}

// verifyViewsResolve EXPLAINs every restored view in one client
// invocation. EXPLAIN resolves the view and checks the definer's rights
// without reading a row (measured: a rights gap reports ERROR 1356, a
// missing definer ERROR 1449), and the client stops at the first failure,
// whose message names the broken view.
func verifyViewsResolve(ctx context.Context, c *core, user, database string) *protoError {
	views, perr := runRows(ctx, c, user, fmt.Sprintf(
		"SELECT table_name FROM information_schema.views WHERE table_schema = '%s' ORDER BY table_name",
		database))
	if perr != nil {
		return perr
	}
	if len(views) == 0 {
		return nil
	}
	var b strings.Builder
	for _, v := range views {
		// View names come from restored content: quote them as
		// identifiers, doubling embedded backticks.
		fmt.Fprintf(&b, "EXPLAIN SELECT * FROM `%s`.`%s`; ", database, strings.ReplaceAll(v, "`", "``"))
	}
	val, _, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"mysql", "-h", "127.0.0.1", "-u", user, "-N", "-B", "-e", b.String()},
	})
	if perr != nil {
		return perr
	}
	if val.ExitCode != 0 {
		return protoErr("restore_failed", false, "restored view is not usable: %s", firstLine(stderr))
	}
	return nil
}

// verifyDatabaseReachable requires at least one restored account (or role)
// to hold a privilege that reaches the restored database: a grant scoped
// to it at the schema, table, column, or routine level, or a global
// non-USAGE privilege. Grants are database-scoped, so a users script
// exported from production names the production database — restored under
// a different name, nothing can reach the target and this gate fires. The
// message says how to fix it.
func verifyDatabaseReachable(ctx context.Context, c *core, user, database string) *protoError {
	query := fmt.Sprintf(
		"SELECT COUNT(*) FROM ("+
			"SELECT grantee FROM information_schema.schema_privileges WHERE table_schema = '%[1]s' UNION ALL "+
			"SELECT grantee FROM information_schema.table_privileges WHERE table_schema = '%[1]s' UNION ALL "+
			"SELECT grantee FROM information_schema.column_privileges WHERE table_schema = '%[1]s' UNION ALL "+
			"SELECT CONCAT('''', user, '''@''', host, '''') FROM mysql.procs_priv WHERE db = '%[1]s' UNION ALL "+
			"SELECT grantee FROM information_schema.user_privileges WHERE privilege_type <> 'USAGE'"+
			") g WHERE g.grantee NOT LIKE '''root''@%%' AND g.grantee NOT LIKE '''mysql.%%'",
		database)
	rows, perr := runRows(ctx, c, user, query)
	if perr != nil {
		return perr
	}
	if len(rows) != 1 || rows[0] == "0" {
		return protoErr("restore_failed", false,
			"no restored account can reach database %s: grants are database-scoped, so the drill "+
				"must restore under the database name the users script grants on — set options.database "+
				"to the source database name", database)
	}
	return nil
}

// nameList joins names for a protocol message, capped so one badly seeded
// backup cannot inflate the error field.
func nameList(names []string, limit int) string {
	if len(names) <= limit {
		return firstLine([]byte(strings.Join(names, ", ")))
	}
	head := strings.Join(names[:limit], ", ")
	return firstLine([]byte(head)) + " and " + strconv.Itoa(len(names)-limit) + " more"
}
