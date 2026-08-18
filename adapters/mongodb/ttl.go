package main

import (
	"context"
	"strings"
)

// ttl.go stops the sandbox from expiring the artifact it was handed.
//
// MongoDB deletes documents past a TTL index's expiry from a background
// thread, and that thread's first pass lands about a minute after mongod
// starts (measured). A restored backup whose documents are already past
// their expiry — an hour-long session TTL in yesterday's backup, a
// ninety-day audit collection in a backup older than that — therefore
// loses them mid-drill, in a single pass, while mongorestore reports
// every document restored successfully.
//
// Measured on the verified image: 500 of 500 expired documents deleted in
// one pass, a collection without a TTL index untouched, and nothing
// anywhere reporting a loss. This adapter runs no document census, so the
// drill would simply have proven less than the backup holds.
//
// Worse than the loss is what it depends on. The pass fires on the
// server's clock, not the drill's, so a small backup that finishes inside
// the first minute sees its data and a production-sized one does not —
// the same backup, the same drill, two answers. Evidence that depends on
// how long a restore took is not evidence, so the drill disables the
// thread before restoring rather than racing it.
//
// The pin is a runtime parameter rather than a mongod flag because the
// drill config supplies the sandbox command: the adapter never sees the
// server's argv. It lasts as long as the server, which is the drill.

// ttlPinEval disables the TTL monitor and prints the command's own ok
// field, so the check rests on the server saying yes rather than on the
// absence of a complaint. An unrecognized parameter makes mongosh exit
// non-zero with the server's message on stderr (measured).
const ttlPinEval = `print(db.adminCommand({setParameter: 1, ttlMonitorEnabled: false}).ok);`

// pinTTLMonitor stops MongoDB from deleting expired documents for the
// life of the sandbox, and returns what it measured.
//
// A refusal here fails the drill. The alternative — carry on and note it
// — would hand back a record whose content depends on the clock, which is
// the one thing this product must never produce; and because the message
// names the parameter, a server that renamed or removed it says so out
// loud instead of costing an operator a day. The code follows the
// prometheus adapter's precedent for a sandbox image that cannot give a
// drill what it needs.
func pinTTLMonitor(ctx context.Context, c *core) (float64, *protoError) {
	val, stdout, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"mongosh", "--quiet", "--norc",
			"--host", "127.0.0.1", "--port", "27017", "admin", "--eval", ttlPinEval},
	})
	if perr != nil {
		return 0, perr
	}
	if val.ExitCode != 0 || strings.TrimSpace(string(stdout)) != "1" {
		return 0, protoErr("invalid_request", false,
			"the sandbox engine would not disable its TTL monitor "+
				"(setParameter ttlMonitorEnabled): %s — a restored backup whose documents "+
				"are past a TTL index's expiry loses them to a background pass about a "+
				"minute in (measured), so the drill would prove whatever survived the clock "+
				"rather than what the backup holds", firstLine(stderr))
	}
	return val.DurationSeconds, nil
}
