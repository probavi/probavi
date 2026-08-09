package main

import (
	"context"
	"strconv"
	"strings"
)

// users.go replays and verifies the account layer a database dump does not
// restore on its own. MongoDB keeps users and roles in the admin database
// (admin.system.users / admin.system.roles); a per-database archive carries
// them only when it was taken with --dumpDbUsersAndRoles, and mongorestore
// puts them back only when it is asked with --restoreDbUsersAndRoles. The
// sharp edge measured on a real server: an archive that *does* carry the
// account layer restores without it silently — exit 0, every collection in
// place, zero users — so the operator did everything right on the backup
// side and the drill still proved only half of the recovery.
//
// The mongodump_with_users kind closes that: it asks for the accounts and
// then refuses to pass unless they actually arrived and resolve.

// usersRestoreFlags are the flags that make mongorestore replay the
// account layer. --db is not optional: mongorestore refuses the request
// without it ("cannot use --restoreDbUsersAndRoles without a specified
// database"), because the archive's users belong to one database.
func usersRestoreFlags(database string) []string {
	return []string{"--restoreDbUsersAndRoles", "--db", database}
}

// orphanedRoleRefsEval lists role references that no longer resolve:
// every role a restored user holds, and every role a restored role
// inherits, asked back from the server with rolesInfo. Asking the server
// rather than comparing against a hardcoded list is what keeps built-in
// roles (read, readWrite, …) out of the result — they resolve, so they
// never look orphaned, and the check keeps working when a server version
// adds new ones.
//
// This is the failure a per-database account restore leaves behind: a user
// on the restored database may hold a role defined in another database
// (allowed by MongoDB), which a single-database archive does not carry.
// The user restores, the role does not, and the privileges it granted are
// silently gone — measured: usersInfo reports the user with ok:1 and zero
// inherited privileges, so nothing but this check notices.
const orphanedRoleRefsEval = `const A = db.getSiblingDB("admin");
const refs = [];
A.system.users.find({}, {db:1, user:1, roles:1}).forEach(u =>
  (u.roles || []).forEach(r => refs.push({holder: "user " + u.db + "." + u.user, db: r.db, role: r.role})));
A.system.roles.find({}, {db:1, role:1, roles:1}).forEach(x =>
  (x.roles || []).forEach(r => refs.push({holder: "role " + x.db + "." + x.role, db: r.db, role: r.role})));
const seen = {};
const bad = [];
for (const r of refs) {
  const k = r.db + " " + r.role;
  if (seen[k] === undefined) {
    const res = db.getSiblingDB(r.db).runCommand({rolesInfo: {role: r.role, db: r.db}});
    seen[k] = !!(res.ok && res.roles && res.roles.length);
  }
  if (!seen[k]) bad.push(r.holder + " -> " + r.db + "." + r.role);
}
print(bad.join("\n"));`

// userCountEval counts the accounts the restored database ends up with.
const userCountEval = `print(db.runCommand({usersInfo: 1}).users.length);`

// verifyAccountLayer is the gate that distinguishes the
// mongodump_with_users kind: after the restore it fails the provision
// unless the account layer is actually present and coherent. Without it an
// archive that carries no accounts — or one whose users point at roles it
// left behind — would still pass, which is the defect the kind exists to
// close, one level down.
func verifyAccountLayer(ctx context.Context, c *core, database string) *protoError {
	count, perr := evalLines(ctx, c, database, userCountEval, "account-layer check")
	if perr != nil {
		return perr
	}
	if len(count) != 1 {
		return protoErr("restore_failed", false,
			"account-layer check returned no answer for database %s", database)
	}
	n, err := strconv.Atoi(count[0])
	if err != nil {
		return protoErr("restore_failed", false,
			"account-layer check returned %s, not a count", firstLine([]byte(count[0])))
	}
	if n == 0 {
		return protoErr("restore_failed", false,
			"restored database %s has no user accounts: the archive carried none for it — "+
				"take it with mongodump --dumpDbUsersAndRoles, or use the mongodump kind, "+
				"which does not claim to restore accounts", database)
	}

	orphans, perr := evalLines(ctx, c, "admin", orphanedRoleRefsEval, "orphaned-role check")
	if perr != nil {
		return perr
	}
	if len(orphans) > 0 {
		return protoErr("restore_failed", false,
			"restored principals reference roles that do not exist: %s", nameList(orphans, 5))
	}
	return nil
}

// evalLines runs one mongosh expression against a database and returns its
// non-empty output lines. A check that cannot run is a failed drill, not a
// passed one: the kind cannot prove its claim either way.
func evalLines(ctx context.Context, c *core, database, eval, what string) ([]string, *protoError) {
	val, stdout, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"mongosh", "--quiet", "--norc",
			"--host", "127.0.0.1", "--port", "27017", database, "--eval", eval},
	})
	if perr != nil {
		return nil, perr
	}
	if val.ExitCode != 0 {
		return nil, protoErr("restore_failed", false, "%s failed: %s", what, verdictLine(stderr))
	}
	var lines []string
	for _, line := range strings.Split(string(stdout), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines, nil
}

// nameList joins names for a protocol message, capped so one badly seeded
// backup cannot inflate the error field.
func nameList(names []string, limit int) string {
	if len(names) <= limit {
		return firstLine([]byte(strings.Join(names, "; ")))
	}
	head := strings.Join(names[:limit], "; ")
	return firstLine([]byte(head)) + " and " + strconv.Itoa(len(names)-limit) + " more"
}
