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
