package evidence

// witness.go implements evidence-schema.md §9.2: the second optional
// input to verification.
//
// §9.1's anchor closes truncation for whoever kept the anchor and closes
// nothing for a reader, because an anchor is a value the operator wrote
// down and could as easily have invented. A witness is a party other than
// the operator who attested the head at a moment, and what makes that
// stronger is the second party rather than a second signature.
//
// The verifier never parses an attestation. It runs a command the
// operator names and reads the exit status, which keeps every receipt
// format out of this contract and keeps a certificate chain, a trust
// store and a revocation question out of the tool an auditor runs on a
// log they do not trust (§9.2.1).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// WitnessTimeout bounds one witness command (§9.2.2). Deliberately not
// configurable: a witness is one network round trip, and a verifier that
// could hang forever on one is a verifier an operator stops scheduling.
const WitnessTimeout = 60 * time.Second

// witnessRefused is the one non-zero exit status that is a verdict rather
// than a failure to run (§9.2.2).
const witnessRefused = 1

// ErrWitnessFailed reports §9.2.2's third row — a witness that could not
// answer. A usage error rather than a verdict, exactly as a malformed
// anchor is: an unreachable witness says nothing about the log, and a
// verifier that turned that into INVALID would teach a reader that
// INVALID sometimes means nothing.
var ErrWitnessFailed = errors.New("witness could not answer")

// Witness is what a verifier reports about §9.2's input.
//
// AttestedAt is what the witness said rather than a measurement: a
// verifier does not check it against its own clock, because
// second-guessing the time would be re-deriving a judgement it
// deliberately does not hold.
type Witness struct {
	Command    string
	Attested   bool
	AttestedAt *string
}

// Witnessed applies §9.2 to a finished result: it asks the command where
// §9.2.3 says to ask, and assigns the verdict §9.2.2 gives.
//
// An empty command leaves the result untouched, and so does an INVALID
// one — a run that returned INVALID is never a source of an anchor
// (§9.1), so there is nothing worth attesting and the command is not run.
func Witnessed(ctx context.Context, res *Result, command string) error {
	if command == "" || res.Status == StatusInvalid {
		return nil
	}
	w, detail, err := askWitness(ctx, command, res.Head)
	if err != nil {
		return err
	}
	res.Witness = w
	if !w.Attested {
		res.Status = StatusInvalid
		res.FailedLine = 0
		res.Reason = witnessReason(command, res.Head, detail)
	}
	return nil
}

// witnessReason names the head the witness refused, because that is the
// value a reader takes to the witness to check for themselves.
func witnessReason(command string, head Head, detail string) string {
	reason := fmt.Sprintf("witness %s holds no attestation for head %s", command, head)
	if detail != "" {
		reason += ": " + detail
	}
	return reason
}

// askWitness runs the command under §9.2.2's contract. The second return
// is the first line of stderr, carried into the reason by the caller; it
// never reaches stdout, where only the attested instant may appear.
func askWitness(ctx context.Context, command string, head Head) (*Witness, string, error) {
	ctx, cancel := context.WithTimeout(ctx, WitnessTimeout)
	defer cancel()

	// Exactly two argv elements and no shell: a witness needing more is
	// wrapped in a script. A verifier expanding a template here would be
	// running whatever a flag value happened to spell.
	cmd := exec.CommandContext(ctx, command, head.String())
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	// Stdin left nil, which is the empty, closed input §9.2.2 requires.
	err := cmd.Run()
	detail := firstLine(stderr.String())

	if err == nil {
		return &Witness{Command: command, Attested: true, AttestedAt: attestedAt(stdout.String())}, "", nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == witnessRefused {
		return &Witness{Command: command, Attested: false}, detail, nil
	}
	return nil, "", fmt.Errorf("%w: %s: %w%s", ErrWitnessFailed, command, err, parenthesised(detail))
}

func parenthesised(detail string) string {
	if detail == "" {
		return ""
	}
	return " (" + detail + ")"
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(line)
}

// attestedAt reads the one line §9.2.2 allows on stdout. Anything that is
// not an RFC 3339 instant — including nothing — reads as attested with
// the time unstated: the exit status already carried the answer, and
// failing the run over an unreadable extra would make the flag harder to
// adopt than the risk warrants.
func attestedAt(out string) *string {
	line := firstLine(out)
	if line == "" {
		return nil
	}
	if _, err := time.Parse(time.RFC3339, line); err != nil {
		return nil
	}
	return &line
}
