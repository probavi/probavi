// Command probavi-adapter-mysql is the MySQL engine adapter for Probavi,
// implementing probavi-adapter/0 (docs/adapter-protocol.md).
//
// Like the postgres adapter it is deliberately self-contained: standard
// library only, no imports from the Probavi core — an adapter must be
// writable from the protocol document alone.
package main

import (
	// Zone names resolve from an embedded database, so a drill never
	// fails because the host image ships no /usr/share/zoneinfo
	// (see zone.go). Standard library; costs ~450 KB of binary.
	_ "time/tzdata"

	"context"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	os.Exit(run(os.Stdin, os.Stdout, os.Stderr))
}

// run speaks one protocol operation over the given streams and returns the
// process exit code: 0 whenever a final response was written (§2.3).
func run(stdin io.Reader, stdout, stderr io.Writer) int {
	logger := slog.New(slog.NewTextHandler(stderr, nil))
	// §2.4: SIGTERM means stop issuing sandbox calls and answer
	// "cancelled"; the core force-kills after its grace period.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	c, req, perr := accept(stdin, stdout)
	if perr != nil {
		if c == nil {
			logger.Error("no usable request on stdin")
			return 1
		}
		return c.finishError(perr)
	}

	var payload any
	switch req.Op {
	case "probe":
		payload = probePayload()
	case "provision":
		payload, perr = opProvision(ctx, c, req.Payload, logger)
	case "healthcheck":
		payload, perr = opHealthcheck(ctx, c, req.Payload)
	case "teardown":
		// Everything this adapter creates lives inside the sandbox, which
		// the provider destroys; there is nothing external to release.
		payload = map[string]bool{"released": true}
	default:
		perr = protoErr("invalid_request", false, "unknown op: %s", req.Op)
	}
	if perr != nil {
		return c.finishError(perr)
	}
	return c.finishOK(payload)
}
