package adapter

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// mutation_test.go holds the cases a mutation run found missing: each one
// is a change to the code that every other test accepted. They are here
// together because that is what they have in common — a gap in what the
// suite asserts rather than a gap in what it covers.

// echoRequestScript answers provision with a final error carrying what the
// request actually said, so a test can assert what reached the adapter
// rather than what the core believed it sent. Quotes are stripped because
// the message crosses the protocol as a JSON string.
const echoRequestScript = prelude +
	`PARAMS=$(printf '%s' "$REQ" | sed -n 's/.*"params":\({[^}]*}\).*/\1/p' | tr -d '"')` + "\n" +
	`CREDS=$(printf '%s' "$REQ" | sed -n 's/.*"credential_env":\(\[[^]]*\]\).*/\1/p' | tr -d '"')` + "\n" +
	`OPTS=$(printf '%s' "$REQ" | sed -n 's/.*"options":\({[^}]*}\).*/\1/p' | tr -d '"')` + "\n" +
	`SCRATCH=$(printf '%s' "$REQ" | sed -n 's/.*"scratch_dir":"\([^"]*\)".*/\1/p')` + "\n" +
	`printf '{"protocol":"probavi-adapter/0","request_id":"%s","ok":false,"error":{"code":"invalid_request",` +
	`"message":"params=%s creds=%s opts=%s scratch=%s","retryable":false}}\n' ` +
	`"$RID" "$PARAMS" "$CREDS" "$OPTS" "$SCRATCH"` + "\n"

// TestTheRequestReachesTheAdapterAsTheDrillDeclaredIt: normalising the
// request fills in what §6.2 requires to be present, and must never
// replace what the drill actually said. A drill that names a source
// parameter or a credential variable has named it for the adapter, and an
// adapter that receives an empty object instead restores from a
// configuration nobody wrote.
func TestTheRequestReachesTheAdapterAsTheDrillDeclaredIt(t *testing.T) {
	r := fakeRunner(t, echoRequestScript, nil, nil)
	req := &ProvisionRequest{
		Source: ProvisionSource{
			Kind:          "file",
			Path:          "/backups/orders.dump",
			Params:        map[string]string{"wal_dir": "/backups/wal"},
			CredentialEnv: []string{"PGPASSWORD"},
		},
		Options: map[string]string{"database": "orders"},
		Sandbox: SandboxInfo{ScratchDir: "/scratch"},
	}
	_, err := r.Provision(context.Background(), req, &fakeVerbs{})
	aerr := asAdapterError(t, err)
	for _, want := range []string{
		"params={wal_dir:/backups/wal}", "creds=[PGPASSWORD]",
		"opts={database:orders}", "scratch=/scratch",
	} {
		if !strings.Contains(aerr.Message, want) {
			t.Errorf("the adapter received %q, want it to carry %q", aerr.Message, want)
		}
	}
}

// TestWhatTheDrillLeftUnsaidArrivesAsTheProtocolRequires: §6.2 gives the
// three collections a shape even when the drill named nothing, and the
// scratch directory a default, so an adapter never has to tell "absent"
// from "empty".
func TestWhatTheDrillLeftUnsaidArrivesAsTheProtocolRequires(t *testing.T) {
	r := fakeRunner(t, echoRequestScript, nil, nil)
	req := &ProvisionRequest{Source: ProvisionSource{Kind: "file", Path: "/backups/orders.dump"}}
	_, err := r.Provision(context.Background(), req, &fakeVerbs{})
	aerr := asAdapterError(t, err)
	for _, want := range []string{"params={}", "creds=[]", "opts={}", "scratch=/tmp"} {
		if !strings.Contains(aerr.Message, want) {
			t.Errorf("the adapter received %q, want it to carry %q", aerr.Message, want)
		}
	}
}

// TestPutFileNeedsBothItsPaths: §4.2 defines put_file by the two paths it
// moves between, and a call missing either is malformed. The message is
// asserted as well as the code, because the guard that follows this check
// refuses an unknown source path with the same code — a verdict reached
// there says nothing about whether the arguments were read at all.
func TestPutFileNeedsBothItsPaths(t *testing.T) {
	const sourcePath = "/backups/orders.dump"
	for name, args := range map[string]string{
		"no source path": `{"source_path":"","dest_path":"/tmp/x"}`,
		// The source the drill named, so the guard would let it through:
		// what refuses this call is the missing destination, nothing else.
		"no dest path":                `{"source_path":"` + sourcePath + `","dest_path":""}`,
		"neither path":                `{}`,
		"args that are not an object": `"not an object"`,
	} {
		t.Run(name, func(t *testing.T) {
			call := `{"call_id":"c1","verb":"put_file","args":` + args + `}`
			r := fakeRunner(t, relayScript(call), nil, nil)
			verbs := &fakeVerbs{}
			_, err := r.Provision(context.Background(),
				&ProvisionRequest{Source: ProvisionSource{Kind: "file", Path: sourcePath}}, verbs)
			aerr := asAdapterError(t, err)
			if aerr.Code != CodeInvalidRequest {
				t.Errorf("code = %q, want %q for a malformed put_file", aerr.Code, CodeInvalidRequest)
			}
			if !strings.Contains(aerr.Message, "malformed put_file args") {
				t.Errorf("message = %q, want the arguments named as what was wrong", aerr.Message)
			}
			if len(verbs.putCalls) != 0 {
				t.Errorf("the sandbox was asked to move %+v for a call that named no path", verbs.putCalls)
			}
		})
	}
}

// TestATimingExactlyAtTheLimitIsAccepted: the bound is the largest
// duration an evidence record can carry, not the first one it cannot.
func TestATimingExactlyAtTheLimitIsAccepted(t *testing.T) {
	if err := validateTimingSeconds("restore", maxTimingSeconds); err != nil {
		t.Errorf("a timing exactly at the limit: %v, want it accepted", err)
	}
	if err := validateTimingSeconds("restore", maxTimingSeconds*1.001); err == nil {
		t.Error("a timing past the limit was accepted; the record cannot represent it")
	}
	if err := validateTimingSeconds("restore", 0); err != nil {
		t.Errorf("a phase that took no measurable time: %v, want it accepted", err)
	}
}

// TestALogLineIsKeptToItsCap: adapter stderr reaches the drill log, so a
// line longer than the cap is kept to it and the rest is read out of the
// pipe rather than left to block the adapter. The boundary is what is
// worth pinning, and it carries one oddity worth stating: the budget
// counts the line's terminator, so a line whose text exactly fills the
// cap keeps all its text and is still reported as truncated.
func TestALogLineIsKeptToItsCap(t *testing.T) {
	const budget = 16
	for name, tc := range map[string]struct {
		in        string
		want      string
		truncated bool
		// wantErr is the reader's own error, returned with the line in
		// hand: a final line with no terminator is still delivered.
		wantErr error
	}{
		"below the cap": {"short line\n", "short line", false, nil},
		"text one byte short of the cap": {
			strings.Repeat("x", budget-1) + "\n", strings.Repeat("x", budget-1), false, nil,
		},
		"text exactly at the cap": {
			strings.Repeat("x", budget) + "\n", strings.Repeat("x", budget), true, nil,
		},
		"text one byte over": {
			strings.Repeat("x", budget+1) + "\n", strings.Repeat("x", budget), true, nil,
		},
		"far over the cap": {
			strings.Repeat("x", budget*100) + "\n", strings.Repeat("x", budget), true, nil,
		},
		"no terminator at all": {
			strings.Repeat("x", budget+5), strings.Repeat("x", budget), true, io.EOF,
		},
	} {
		t.Run(name, func(t *testing.T) {
			// A reader smaller than the line proves the loop keeps reading
			// past its own buffer: an unread pipe is what deadlocks a drill.
			br := bufio.NewReaderSize(strings.NewReader(tc.in), 8)
			line, truncated, err := readCappedLine(br, budget)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("readCappedLine error = %v, want %v", err, tc.wantErr)
			}
			if line != tc.want || truncated != tc.truncated {
				t.Errorf("readCappedLine = %q (truncated %v), want %q (truncated %v)",
					line, truncated, tc.want, tc.truncated)
			}
			if len(line) > budget {
				t.Errorf("kept %d bytes of a %d-byte budget", len(line), budget)
			}
		})
	}
}

// TestEveryWayAPipeClosesIsRecognised: the three errors mean the same
// thing — the adapter's end of stdin is gone — and each arrives from a
// different layer: the kernel's EPIPE, the io package's own sentinel, and
// a file closed under us. A write failure this does not recognise is
// reported as a transport failure, which would blame the core for an
// adapter that simply exited first.
func TestEveryWayAPipeClosesIsRecognised(t *testing.T) {
	for name, err := range map[string]error{
		"the kernel's EPIPE":     syscall.EPIPE,
		"io.ErrClosedPipe":       io.ErrClosedPipe,
		"a file closed under us": os.ErrClosed,
		"wrapped":                fmt.Errorf("write request: %w", syscall.EPIPE),
	} {
		t.Run(name, func(t *testing.T) {
			if !closedPipe(err) {
				t.Errorf("closedPipe(%v) = false, want it recognised", err)
			}
		})
	}
	for name, err := range map[string]error{
		"nothing at all":     nil,
		"an unrelated error": errors.New("boom"),
		"a permission error": os.ErrPermission,
	} {
		t.Run(name, func(t *testing.T) {
			if closedPipe(err) {
				t.Errorf("closedPipe(%v) = true, want only a closed pipe to match", err)
			}
		})
	}
}

// probeBothVersions is a probe payload from an adapter that speaks the
// current version as well as the floor, so negotiation has something to
// choose. Every other fake adapter in this package declares the floor
// alone, which is why the version a request carries was invisible: floor
// and negotiated were the same string, and sending every operation at
// either one looked identical on the wire.
const probeBothVersions = `{"name":"fake","adapter_version":"0.0.1",` +
	`"protocol_versions":["probavi-adapter/1","probavi-adapter/0"],"engine":{"name":"fakedb"},` +
	`"sources":[{"kind":"file","capabilities":{"pitr":true}}],"sql_runner":{"argv":["cat"],"env":{}},` +
	`"verbs_required":["exec","put_file"]}`

// echoProtocolScript accepts a probe that arrived at the floor and refuses
// everything else with a final error naming the version and the operation
// that carried it.
//
// A response has to repeat its request's version or the core calls it a
// violation, so the script echoes what it received: a wrong version then
// reaches the test as a message it can read rather than as a crash whose
// text says nothing about which version was wrong.
const echoProtocolScript = prelude +
	`OP=$(printf '%s' "$REQ" | sed -n 's/.*"op":"\([^"]*\)".*/\1/p')` + "\n" +
	`PROTO=$(printf '%s' "$REQ" | sed -n 's/.*"protocol":"\([^"]*\)".*/\1/p')` + "\n" +
	`if [ "$OP" = probe ] && [ "$PROTO" = probavi-adapter/0 ]; then` + "\n" +
	`printf '{"protocol":"%s","request_id":"%s","ok":true,"payload":` + probeBothVersions + `}\n' "$PROTO" "$RID"` + "\n" +
	`else` + "\n" +
	`printf '{"protocol":"%s","request_id":"%s","ok":false,"error":{"code":"invalid_request",` +
	`"message":"op=%s protocol=%s","retryable":false}}\n' "$PROTO" "$RID" "$OP" "$PROTO"` + "\n" +
	`fi` + "\n"

// TestTheProbeAsksAtTheFloorAndTheRestSpeakWhatWasNegotiated: §8 gives the
// probe a version of its own. It is the one request sent before anything is
// known about the adapter, so it goes out at the floor — a version every
// adapter must accept — and what negotiation chooses from its answer
// governs every request after it.
//
// Both halves are asserted on the wire rather than through Protocol(),
// because the runner reporting the right version and the request carrying
// it are two different statements: sending every operation at the floor,
// or the probe at whatever was last negotiated, leaves Protocol() correct
// and the adapter receiving the wrong thing.
func TestTheProbeAsksAtTheFloorAndTheRestSpeakWhatWasNegotiated(t *testing.T) {
	r := fakeRunner(t, echoProtocolScript, nil, nil)
	ctx := context.Background()

	if _, err := r.Probe(ctx); err != nil {
		t.Fatalf("Probe: %v — the first probe must go out at the floor", err)
	}
	if r.Protocol() != ProtocolVersion {
		t.Fatalf("Protocol() = %q, want %q negotiated from the probe", r.Protocol(), ProtocolVersion)
	}
	// The second probe is the half a single probe cannot show: now that
	// negotiation has raised the version, a probe must still ask at the
	// floor. The script refuses any other version, so success is the
	// assertion.
	if _, err := r.Probe(ctx); err != nil {
		t.Errorf("second Probe: %v — a probe asks at the floor however negotiation went", err)
	}

	_, err := r.Healthcheck(ctx, &Connection{}, nil, &fakeVerbs{})
	aerr := asAdapterError(t, err)
	if want := "op=healthcheck protocol=" + ProtocolVersion; !strings.Contains(aerr.Message, want) {
		t.Errorf("the adapter received %q, want %q", aerr.Message, want)
	}
}

// TestAnAdapterWhoseChildHoldsStderrOpenFails: the adapter exits cleanly
// and on time, and leaves a background child holding the stderr pipe. Its
// exit status says nothing was wrong — the process that had to be killed to
// get to EOF was not the one the core waited for — so the fact that the
// grace period ran out is the only thing left to fail on.
//
// Letting it pass would sign a record for a drill whose adapter never
// finished speaking: the log keeps whatever reached the pipe before the
// deadline, and the reason it failed is typically the last thing written.
func TestAnAdapterWhoseChildHoldsStderrOpenFails(t *testing.T) {
	// The child outlives the grace period by two orders of magnitude, so
	// the drain reaching EOF on its own would be a bug, not a slow machine.
	script := prelude + probeFinal(probePayload) + "(sleep 10) &\nexit 0\n"
	r := fakeRunner(t, script, nil, &Options{Grace: 100 * time.Millisecond})

	_, err := r.Probe(context.Background())
	aerr := asAdapterError(t, err)
	if aerr.Code != CodeAdapterCrash {
		t.Errorf("code = %q, want %q", aerr.Code, CodeAdapterCrash)
	}
	if !strings.Contains(aerr.Message, "lingered past the grace period") {
		t.Errorf("message = %q, want it to name the grace period as what ran out", aerr.Message)
	}
}

// TestAGraceTheCallerSetIsKept: the default fills in for a grace of zero or
// less, and for nothing else. The boundary is pinned at one nanosecond
// rather than at a plausible setting because that is where the comparison
// lives: every realistic grace is far enough above zero that a guard
// reaching one step further would go unnoticed, and a runner that quietly
// waits ten seconds where the caller asked for less has taken the drill's
// wall-clock bound away from it.
func TestAGraceTheCallerSetIsKept(t *testing.T) {
	if got := newRunner("/nonexistent", nil, &Options{Grace: 1}).opts.Grace; got != 1 {
		t.Errorf("grace = %v, want the one nanosecond the caller asked for", got)
	}
	if got := newRunner("/nonexistent", nil, &Options{Grace: -1}).opts.Grace; got != defaultGrace {
		t.Errorf("grace = %v, want the default %v where the caller gave none", got, defaultGrace)
	}
}

// readerBytes is the size of the bufio buffer the cap boundaries below read
// through. bufio rounds any smaller request up to this, its own minimum,
// and these cases turn on where a chunk boundary falls relative to the cap,
// so the number is named here rather than inherited from a request bufio
// may not honour.
const readerBytes = 16

// TestTheCapIsCountedToTheByte: readCappedLine keeps the first limit bytes
// of a line, and the two boundaries deciding whether it keeps all of them
// only show up when the cap and the reader's buffer disagree — which is the
// normal case in the drill log, where the cap is a frame limit and the
// buffer is 64 KiB.
//
// Both were accepted with the count off by one byte: the last byte of a
// truncated line dropped, and a line that lost nothing reported truncated.
// Neither loses much on its own; what they cost is the ability to read the
// log and know whether anything is missing.
func TestTheCapIsCountedToTheByte(t *testing.T) {
	t.Run("a cap one byte past a chunk boundary still keeps that byte", func(t *testing.T) {
		const limit = readerBytes + 1
		br := bufio.NewReaderSize(strings.NewReader(strings.Repeat("x", limit*3)+"\n"), readerBytes)
		line, truncated, err := readCappedLine(br, limit)
		if err != nil || !truncated {
			t.Fatalf("readCappedLine err = %v, truncated = %v; want a truncated line and no error",
				err, truncated)
		}
		if len(line) != limit {
			t.Errorf("kept %d bytes of a %d-byte cap — the cap is how many bytes are kept, "+
				"not roughly how many", len(line), limit)
		}
	})
	t.Run("text filling the cap exactly, with no terminator, lost nothing", func(t *testing.T) {
		// The complement of the oddity TestALogLineIsKeptToItsCap states:
		// the cap counts the terminator, so text that exactly fills it is
		// truncated when a newline follows and intact when the pipe ends.
		const limit = readerBytes
		br := bufio.NewReaderSize(strings.NewReader(strings.Repeat("x", limit)), readerBytes)
		line, truncated, err := readCappedLine(br, limit)
		if !errors.Is(err, io.EOF) {
			t.Errorf("err = %v, want io.EOF with the line in hand", err)
		}
		if line != strings.Repeat("x", limit) || truncated {
			t.Errorf("readCappedLine = %q (truncated %v), want the line whole and truncated false",
				line, truncated)
		}
	})
}
