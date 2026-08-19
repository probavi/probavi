package main

// retention.go stops the sandbox from applying its own retention policy to
// the artifact it was handed.
//
// An InfluxDB bucket carries a retention period, and `influx restore`
// restores it along with the data (measured: a bucket backed up at
// `1h0m0s` comes back at `1h0m0s`). The restored server then enforces it
// exactly as a production server would — on data it has just been handed,
// which is the one place the policy has nothing to say. The points a
// backup holds were inside the bucket's retention when `influx backup`
// ran; an artifact holding points past it cannot even be written, because
// the write path rejects them outright:
//
//	422 Unprocessable Entity: partial write: dropped 1 points outside
//	retention policy of duration 1h0m0s
//
// Time then passes — or the operator shortens the bucket — and by the
// drill the same points are outside the window.
//
// Measured on the baseline image, restoring a backup of a one-hour bucket
// holding seven points spread over three hours, beside a control bucket
// with infinite retention:
//
//	                        at the restore   one check later
//	metrics (retention 1h)        7                3
//	audit   (retention ∞)         1                1
//	buckets the census sees       5                5
//
// The census is the point. This adapter's completeness verdict compares
// the buckets the instance holds with the ones the manifest names, and a
// bucket that lost every point it had is still a bucket — so the drill
// goes green having proved less than the backup holds. That is the
// false-green half of the class in issue #166.
//
// # Why it is worse than a straight loss
//
// The enforcer runs on a ticker, and the engine's default interval is
// thirty minutes (measured: `check_interval=30m`, and a two-minute drill
// escapes untouched). So whether a drill sees the loss depends on whether
// it outlives one tick — on how long the restore took and how long the
// checks ran. The same backup, drilled twice, can produce two different
// answers, and nothing in either record would say which one happened.
//
// # The pin
//
// Unlike the sibling adapters that inherit a server someone else started,
// this adapter launches `influxd` itself, so the policy is pinned with a
// flag rather than a statement: the enforcer's first tick is moved beyond
// any sandbox's life. Retention stays exactly as the backup declared it —
// `influx bucket list` in a drill reports the operator's own periods — so
// a check reading the bucket's configuration sees the truth. What is
// suspended is only the enforcement, and only inside the sandbox.

// retentionCheckInterval moves the retention enforcer's first tick past
// any drill. The engine logs it as `check_interval=100y`.
//
// Zero would be the obvious way to say "never" and is the one value that
// must not be used: `--storage-retention-check-interval 0` is accepted by
// the flag parser and then kills the server outright — `panic:
// non-positive interval for NewTicker`, no port ever opened (measured).
// The engine's own ceiling is 2562047h, which is what a duration holds in
// nanoseconds; a century is far enough and reads as a decision rather than
// an overflow.
const retentionCheckInterval = "876000h"
