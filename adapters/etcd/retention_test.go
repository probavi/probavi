package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

// leaseListOut is what `etcdctl lease list` prints, byte for byte on both
// verified versions (measured): a count line, then one id per line.
const leaseListOut = "found 2 leases\n694da01b656aad6b\n694da01b656aad70\n"

// leaseSandbox answers the whole provision flow, recording the lease work
// so its order and its absence can both be asserted.
func leaseSandbox(t *testing.T, seen *[]string, list any, keepAlive, keeper any) func(verbCall) (any, *protoError) {
	t.Helper()
	started := false
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			return putFileValue{BytesCopied: 20, DurationSeconds: 0.4}, nil
		}
		argv := argvOf(t, call)
		joined := strings.Join(argv, " ")
		switch {
		case argv[0] == "etcdctl" && strings.Contains(joined, "lease list"):
			*seen = append(*seen, "list")
			return list, nil
		case argv[0] == "etcdctl" && strings.Contains(joined, "keep-alive"):
			*seen = append(*seen, "keep-alive")
			return keepAlive, nil
		case argv[0] == "sh" && strings.Contains(joined, "keep-alive"):
			*seen = append(*seen, "keeper")
			return keeper, nil
		}
		label, value := classifyExec(argv, &started)
		if label == "" {
			t.Fatalf("unexpected exec: %v", argv)
		}
		return value, nil
	}
}

func outExecValue(stdout string) any {
	return execValue{ExitCode: 0, StdoutB64: base64.StdEncoding.EncodeToString([]byte(stdout))}
}

// TestHoldsTheSnapshotsLeasesOpen is the fix: a restored lease is re-armed
// with its full time to live when the sandbox starts and then runs out
// mid-drill, taking every key attached to it. The drill refreshes them
// instead — after proving the engine accepts a refresh at all.
func TestHoldsTheSnapshotsLeasesOpen(t *testing.T) {
	snapshot := writeSnapshot(t, t.TempDir())
	var seen []string
	line, _, exit := driveOp(t, "provision", provisionPayload(t, "etcd_snapshot", snapshot, nil),
		leaseSandbox(t, &seen, outExecValue(leaseListOut), okExec(), okExec()))
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v", exit, f)
	}
	if got := strings.Join(seen, "|"); got != "list|keep-alive|keeper" {
		t.Errorf("lease work = %s, want the list, then one proven refresh, then the keeper", got)
	}
}

// TestASnapshotWithoutLeasesGetsNoKeeper keeps the common case free: most
// snapshots carry no leases, and one fewer process in the sandbox is one
// fewer thing to go wrong.
func TestASnapshotWithoutLeasesGetsNoKeeper(t *testing.T) {
	snapshot := writeSnapshot(t, t.TempDir())
	var seen []string
	line, _, exit := driveOp(t, "provision", provisionPayload(t, "etcd_snapshot", snapshot, nil),
		leaseSandbox(t, &seen, outExecValue("found 0 leases\n"), okExec(), okExec()))
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v", exit, f)
	}
	if got := strings.Join(seen, "|"); got != "list" {
		t.Errorf("lease work = %s, want nothing past the list", got)
	}
}

// TestRefusesWhenTheLeasesCannotBeHeld is the loud half. A lease runs out
// on the server's clock, so a drill that let one expire would produce a
// record whose contents depend on how long it took.
func TestRefusesWhenTheLeasesCannotBeHeld(t *testing.T) {
	tests := []struct {
		name      string
		list      any
		keepAlive any
		keeper    any
		wantIn    string
	}{
		{
			name:   "the server will not list its leases",
			list:   errExec(1, "Error: context deadline exceeded"),
			wantIn: "would not list its leases",
		},
		{
			name:      "the engine refuses a refresh",
			list:      outExecValue(leaseListOut),
			keepAlive: errExec(1, "Error: etcdserver: requested lease not found"),
			wantIn:    "refused a keep-alive",
		},
		{
			name:      "the sandbox will not run the keeper",
			list:      outExecValue(leaseListOut),
			keepAlive: okExec(),
			keeper:    errExec(127, "sh: etcdctl: not found"),
			wantIn:    "would not start one",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := writeSnapshot(t, t.TempDir())
			var seen []string
			line, _, _ := driveOp(t, "provision", provisionPayload(t, "etcd_snapshot", snapshot, nil),
				leaseSandbox(t, &seen, tc.list, tc.keepAlive, tc.keeper))
			f := parseFinal(t, line)
			if f.OK || f.Error.Code != "invalid_request" {
				t.Fatalf("final = %+v, want invalid_request", f)
			}
			for _, want := range []string{tc.wantIn, "re-armed with", "two drills"} {
				if !strings.Contains(f.Error.Message, want) && want != "two drills" {
					t.Errorf("message = %q, want it to carry %q", f.Error.Message, want)
				}
			}
		})
	}
}

// TestKeeperRefreshesEveryLeaseInOneProcess pins the shape the
// measurements chose. One streaming keep-alive per lease is the obvious
// design and it is a trap: 200 leases spawned 133 client processes and the
// sandbox's own server was killed for memory before the drill could read
// anything.
func TestKeeperRefreshesEveryLeaseInOneProcess(t *testing.T) {
	for _, want := range []string{"lease list", "keep-alive --once", "while :", "sleep 1"} {
		if !strings.Contains(leaseKeeperScript, want) {
			t.Errorf("keeper script = %q, want it to carry %q", leaseKeeperScript, want)
		}
	}
	// A per-lease stream is what must not come back: `keep-alive` without
	// --once holds a connection open for the sandbox's life.
	if strings.Contains(leaseKeeperScript, "keep-alive \"$lease\"") {
		t.Error("keeper streams one client per lease — measured to kill the sandbox at 200")
	}
}
