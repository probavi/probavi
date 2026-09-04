package main

import (
	"context"
	"fmt"
	"strings"
)

// retention.go stops the sandbox from expiring the artifact it was handed.
//
// etcd's data-lifecycle mechanism is not the one this survey expected.
// Auto-compaction is off by default in both verified versions — the
// server logs `"auto-compaction-retention":"0s"` at startup — and it
// removes superseded revisions rather than live keys, so a drill reading
// current values would not notice it either way. That question is closed.
//
// **Leases are the mechanism.** A key attached to a lease exists only
// while somebody keeps renewing it, and a snapshot captures both the key
// and the lease. On restore the lessor re-arms every lease with its full
// TTL — etcd says so itself, in the help text for the flag that exists to
// prevent it: `--experimental-enable-lease-checkpoint` "prevents
// indefinite auto-renewal of long lived leases". So the countdown starts
// again when the sandbox starts, and then it runs out **during the
// drill**.
//
// Measured on both verified versions, a snapshot of 100 plain keys beside
// 100 attached to a twenty-second lease:
//
//	seconds after the restore   plain   leased
//	              1              100      100
//	             20              100      100
//	             27              100        0
//
// The drill reports a successful restore either way, and what a check
// reads depends on when it runs. That is the worst shape this class
// takes: not a loss, but a coin toss — the same backup, drilled twice,
// answering differently for reasons that have nothing to do with the
// backup.
//
// # There is no server-side pin
//
// Searched rather than assumed: the only lease-related flags either
// version offers are the checkpoint pair above, and they make expiry
// *stricter* by persisting the remaining TTL across restarts. Nothing
// suspends the lessor.
//
// What etcd does offer is the mechanism its own clients use: a keep-alive
// refreshes a lease to its full TTL. So the drill holds the artifact's
// leases open for as long as the sandbox lives, which leaves the leases
// exactly as the backup declared them — `lease timetolive` in a drill
// reports the operator's own `granted with TTL(20s)` — and suspends only
// their expiry.
//
// # One keeper, not one per lease
//
// The obvious shape is one streaming `etcdctl lease keep-alive` per
// lease, and it is a trap: measured, 200 leases spawned 133 client
// processes and the sandbox's own server was killed for memory before the
// drill could read anything. A single loop refreshing every lease in turn
// costs one process at a time and holds the same 200 leases indefinitely
// (measured, with the server healthy throughout).
//
// The cost is ~10 ms per lease per sweep — 200 leases sweep in about two
// seconds — which bounds the residual honestly: a snapshot with enough
// leases, or leases short enough, that a sweep cannot outrun the shortest
// TTL will still lose them. That is no worse than what happens today, and
// the README says so rather than implying the keeper is a guarantee.

// leaseKeeperScript refreshes every lease the restored server holds, for
// as long as the sandbox lives.
//
// It lists the leases each time round rather than once at the start: a
// lease revoked by a check should stop being refreshed, and the list is
// one call against a local server.
var leaseKeeperScript = fmt.Sprintf(`(while :; do
  etcdctl --endpoints=%s lease list 2>/dev/null | tail -n +2 | while read -r lease; do
    [ -n "$lease" ] && etcdctl --endpoints=%s lease keep-alive --once "$lease" >/dev/null 2>&1
  done
  sleep 1
done) >/dev/null 2>&1 &`, clientEndpoint, clientEndpoint)

// leaseListHeader is what `etcdctl lease list` prints before the ids, on
// both verified versions: `found N leases`.
const leaseListHeader = "found "

// holdLeases keeps the snapshot's leases from expiring mid-drill, and
// returns what it measured.
//
// A snapshot with no leases needs no keeper and gets none — the common
// case, and one fewer process in the sandbox.
func holdLeases(ctx context.Context, c *core) (leases int, seconds float64, perr *protoError) {
	ids, listSeconds, perr := listLeases(ctx, c)
	if perr != nil {
		return 0, 0, perr
	}
	if len(ids) == 0 {
		return 0, listSeconds, nil
	}

	// Positive evidence before the promise: the engine accepts a refresh
	// for a lease the artifact actually carried. A keeper started without
	// this would be a loop nobody had seen work.
	proof, stderr, perr := execChecked(ctx, c,
		"etcdctl", "--endpoints="+clientEndpoint, "lease", "keep-alive", "--once", ids[0])
	if perr != nil {
		return 0, 0, perr
	}
	if proof.ExitCode != 0 {
		return 0, 0, refusedLeases("the engine refused a keep-alive for lease " + ids[0] +
			": " + firstLine(stderr))
	}

	start, stderr, perr := execChecked(ctx, c, "sh", "-c", leaseKeeperScript)
	if perr != nil {
		return 0, 0, perr
	}
	if start.ExitCode != 0 {
		return 0, 0, refusedLeases("the sandbox would not start one: " + firstLine(stderr))
	}
	return len(ids), listSeconds + proof.DurationSeconds + start.DurationSeconds, nil
}

// listLeases returns the ids of the leases the restored server holds.
func listLeases(ctx context.Context, c *core) ([]string, float64, *protoError) {
	val, stdout, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"etcdctl", "--endpoints=" + clientEndpoint, "lease", "list"}})
	if perr != nil {
		return nil, 0, perr
	}
	if val.ExitCode != 0 {
		return nil, 0, refusedLeases("the restored server would not list its leases: " + firstLine(stderr))
	}
	lines := strings.Split(string(stdout), "\n")
	ids := make([]string, 0, len(lines))
	for _, line := range lines {
		id := strings.TrimSpace(line)
		if id == "" || strings.HasPrefix(id, leaseListHeader) {
			continue
		}
		ids = append(ids, id)
	}
	return ids, val.DurationSeconds, nil
}

// refusedLeases is the verdict when the drill cannot hold the artifact's
// leases open.
//
// A refusal rather than a note: a lease runs out on the server's clock,
// so a drill that let one expire would hand back a record whose contents
// depend on how long the restore and the checks took — the same backup,
// two drills, two answers.
func refusedLeases(reason string) *protoError {
	return protoErr("invalid_request", false,
		"the drill cannot hold the backup's leases open — %s. A restored lease is re-armed with "+
			"its full time to live when the sandbox starts and then runs out mid-drill, taking "+
			"every key attached to it (measured), so the drill would prove whatever survived the "+
			"clock rather than what the backup holds", reason)
}

// keyCensusScript answers how many keys the restored server serves, as a
// bare number.
//
// The extraction happens in the sandbox rather than in Go for two
// reasons. Only `--write-out=fields` answers `--count-only` — the json
// writer refuses the combination outright (measured) — and its output is
// a labelled pair rather than a value. And a census that reads a bare
// count is the shape the protocol's own conformance contract expects:
// §10 drives an adapter against a simulated sandbox where every exec
// answers `1`, so a provision that cannot read that is non-conformant by
// construction. Every other census in this repository has the same shape.
var keyCensusScript = fmt.Sprintf(
	`etcdctl --endpoints=%s get "" --prefix --count-only -w fields | sed -n 's/^"Count" : //p'`,
	clientEndpoint)
