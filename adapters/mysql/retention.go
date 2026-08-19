package main

import (
	"context"
	"strings"
)

// retention.go stops the sandbox from running the backup's own scheduled
// jobs against the artifact it is proving.
//
// MySQL has no per-row expiry, so this engine's instance of the class
// in issue #166 is the event scheduler: a dump taken with `--events`
// carries `CREATE EVENT` statements, and an operator's purge event —
// `DELETE FROM orders WHERE created < NOW() - INTERVAL 90 DAY` is the
// canonical shape — deletes rows in the drill exactly as it does in
// production.
//
// Measured: an artifact of ten rows and one such event, restored into a
// sandbox whose scheduler was running, held **two rows five seconds after
// the restore**. The event arrives ENABLED, because a dump preserves the
// status the backup recorded — and it should, since a check reading
// `information_schema.events` is entitled to see the operator's own
// definitions.
//
// # Whether it runs is a default, and defaults move
//
// This is not hypothetical, and the two families disagree about it today:
//
//	mysql:8.4                       event_scheduler=ON
//	percona/percona-server:8.4.10   event_scheduler=ON
//	mariadb:10.11 … 12.3            event_scheduler=OFF
//
// MySQL turned this on in 8.0 having shipped it off for years, which is
// the whole argument for pinning rather than relying on the answer: a
// drill's independence must not rest on an upstream default. On the
// MariaDB side the same loss is one sandbox parameter away — measured,
// a sandbox started with `--event-scheduler=ON` loses the same eight rows
// in four seconds.
//
// # Two paths, two pins
//
// The logical kinds restore into a server this adapter did not start, so
// the pin is a statement — and it is deterministic, because the events do
// not exist until the dump loads them.
//
// The physical kind restores a data directory that already holds the
// event definitions and then starts the server itself, so a statement
// after startup would be racing the scheduler. There the pin is a startup
// flag, and the same query verifies afterwards that the server agrees.
//
// Either way the artifact is untouched: the events keep the definitions
// and the ENABLED status the backup recorded, and only their execution is
// suspended, for the life of the sandbox.

const (
	// eventSchedulerFlag is what the physical path adds to the server's
	// own argv, where a statement would come too late.
	eventSchedulerFlag = "--event-scheduler=OFF"

	// pinStatement suspends the scheduler on a running server. It is a
	// dynamic global, so it needs no restart.
	pinStatement = "SET GLOBAL event_scheduler = OFF"

	// pinnedQuery answers 1 when the server will not run events.
	//
	// DISABLED counts as pinned and is not an error to leave alone: a
	// server started that way cannot run events and cannot be made to —
	// `SET GLOBAL event_scheduler = OFF` against it fails with ERROR 1290
	// (measured), which would be a refusal over the safest state there is.
	pinnedQuery = "SELECT @@event_scheduler IN ('OFF','DISABLED')"

	// pinnedAnswer is what both queries print when the sandbox will not
	// run the backup's events.
	pinnedAnswer = "1"
)

// prepareEngine waits until the sandbox server answers queries and will
// not run the backup's scheduled jobs. Both are preconditions for the
// restore, and the second has to hold before the dump can create the
// events it suspends.
func prepareEngine(ctx context.Context, c *core, user string) (float64, *protoError) {
	readySeconds, perr := awaitEngine(ctx, c, user)
	if perr != nil {
		return 0, perr
	}
	pinSeconds, perr := pinEventScheduler(ctx, c, user)
	if perr != nil {
		return 0, perr
	}
	return readySeconds + pinSeconds, nil
}

// pinEventScheduler makes sure the sandbox will not run the backup's
// scheduled jobs, and returns what it measured.
//
// It asks before it acts: a server that is already OFF or DISABLED needs
// no statement, and asking first is also what keeps the answer positive —
// the verdict is the server saying it will not run events, never the
// absence of a complaint.
func pinEventScheduler(ctx context.Context, c *core, user string) (float64, *protoError) {
	total, pinned, perr := readSchedulerState(ctx, c, user)
	if perr != nil {
		return 0, perr
	}
	if pinned {
		return total, nil
	}

	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: clientArgv(user, pinStatement)})
	if perr != nil {
		return 0, perr
	}
	total += val.DurationSeconds
	if val.ExitCode != 0 {
		return 0, refusedScheduler(firstLine(stderr))
	}

	seconds, pinned, perr := readSchedulerState(ctx, c, user)
	if perr != nil {
		return 0, perr
	}
	total += seconds
	if !pinned {
		return 0, refusedScheduler("the server still reports the scheduler running")
	}
	return total, nil
}

// readSchedulerState asks whether the server will run events.
func readSchedulerState(ctx context.Context, c *core, user string) (float64, bool, *protoError) {
	val, stdout, stderr, perr := c.exec(ctx, execArgs{Argv: clientArgv(user, pinnedQuery)})
	if perr != nil {
		return 0, false, perr
	}
	if val.ExitCode != 0 {
		return 0, false, refusedScheduler(firstLine(stderr))
	}
	return val.DurationSeconds, strings.TrimSpace(firstLine(stdout)) == pinnedAnswer, nil
}

// clientArgv runs one statement over the loopback connection.
func clientArgv(user, statement string) []string {
	return []string{"mysql", "-h", "127.0.0.1", "-u", user, "-N", "-B", "-e", statement}
}

// refusedScheduler is the verdict when the sandbox will not promise to
// leave the artifact alone.
//
// A refusal rather than a note: an event deletes rows on its own schedule,
// so a drill that let one run would produce a record whose contents depend
// on how long the restore took — two drills of the same backup, two
// answers. That is the one thing this product must never emit.
func refusedScheduler(reason string) *protoError {
	return protoErr("invalid_request", false,
		"the sandbox engine would not suspend its event scheduler (%s): %s — a dump taken with "+
			"--events carries the backup's own scheduled jobs, and a purge event restored into a "+
			"running scheduler deletes rows seconds after the restore (measured), so the drill "+
			"would prove whatever survived the clock rather than what the backup holds",
		pinStatement, reason)
}
