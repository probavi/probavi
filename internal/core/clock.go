package core

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// clockProbeTimeout bounds the one command this file runs. A time daemon
// that does not answer promptly is the same as one that is not there: the
// record says nothing rather than waiting, because a drill must never be
// delayed by a field it is allowed to leave null.
const clockProbeTimeout = 2 * time.Second

// clockSynchronised reports whether the host *believed* its clock was
// synchronised (evidence-schema.md §3).
//
// What it attests is a belief and not a time. A lying or manipulated time
// source produces the same true, which is why the schema narrows this
// field's claim the way it narrows adapter.digest — and why it must never
// be described as doing the outside witness's work, where someone other
// than the host asserts the instant.
//
// nil is an ordinary answer, not a failure: outside systemd there is no
// timedatectl to ask, and a host without a time daemon has nothing to
// believe. Every error path here returns nil, because nearly everything
// else in a record rests on the host clock and none of it is worth failing
// a drill over.
func (d *Drill) clockSynchronised(ctx context.Context) *bool {
	out, err := d.probeClock(ctx)
	if err != nil {
		d.Logger.Debug("clock synchronisation unknown", "err", err)
		return nil
	}
	switch strings.TrimSpace(out) {
	case "yes":
		yes := true
		return &yes
	case "no":
		no := false
		return &no
	default:
		// An answer this code does not understand is not an answer. A
		// future systemd wording must read as "unknown" rather than as
		// "not synchronised", which would be a claim nothing made.
		d.Logger.Debug("clock synchronisation unreadable", "answer", strings.TrimSpace(out))
		return nil
	}
}

// probeClock asks systemd what it believes. It is a field of the drill so
// a test can answer without a time daemon, and so this package has exactly
// one place that shells out for the clock.
func (d *Drill) probeClock(ctx context.Context) (string, error) {
	if d.ProbeClock != nil {
		return d.ProbeClock(ctx)
	}
	probeCtx, cancel := context.WithTimeout(ctx, clockProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(probeCtx, "timedatectl", "show", "-p", "NTPSynchronized", "--value").Output()
	return string(out), err
}
