//go:build integration

package docker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/probavi/probavi/internal/sandbox"
)

// testImage must stay running with its default entrypoint (like a real
// database image does) and must carry a busybox shell for exec assertions.
const testImage = "nginx:1.27-alpine"

// TestDockerLifecycle drives a real container through the full provider
// contract: create, exec (plain, stdin, exit codes), put_file, isolation,
// idempotent destroy.
func TestDockerLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	p := New(nil)

	sbx, err := p.Create(ctx, map[string]string{"image": testImage, "env.PROBAVI_TEST": "1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	destroyed := false
	defer func() {
		if !destroyed {
			dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer dcancel()
			_ = sbx.Destroy(dctx)
		}
	}()

	res, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{"echo", "hello"}})
	if err != nil {
		t.Fatalf("Exec echo: %v", err)
	}
	if res.ExitCode != 0 || strings.TrimSpace(string(res.Stdout)) != "hello" {
		t.Errorf("echo: exit=%d out=%q", res.ExitCode, res.Stdout)
	}

	res, err = sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{"cat"}, Stdin: []byte("piped-data")})
	if err != nil {
		t.Fatalf("Exec cat: %v", err)
	}
	if string(res.Stdout) != "piped-data" {
		t.Errorf("stdin roundtrip = %q", res.Stdout)
	}

	res, err = sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{"sh", "-c", "exit 7"}})
	if err != nil {
		t.Fatalf("Exec exit 7: %v", err)
	}
	if res.ExitCode != 7 {
		t.Errorf("exit code = %d, want 7", res.ExitCode)
	}

	res, err = sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{"printenv", "PROBAVI_TEST"}})
	if err != nil || strings.TrimSpace(string(res.Stdout)) != "1" {
		t.Errorf("env param did not reach the container: out=%q err=%v", res.Stdout, err)
	}

	// Zero-ingress default: network none leaves loopback only.
	res, err = sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{"ls", "/sys/class/net"}})
	if err != nil {
		t.Fatalf("Exec ls net: %v", err)
	}
	if ifaces := strings.Fields(string(res.Stdout)); !slices.Equal(ifaces, []string{"lo"}) {
		t.Errorf("interfaces = %v, want [lo] — the default must be zero network exposure", ifaces)
	}

	hostFile := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(hostFile, []byte("payload-bytes"), 0o600); err != nil {
		t.Fatalf("write host file: %v", err)
	}
	pf, err := sbx.PutFile(ctx, hostFile, sbx.ScratchDir()+"/payload.bin", "0640")
	if err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	if pf.BytesCopied != int64(len("payload-bytes")) {
		t.Errorf("BytesCopied = %d", pf.BytesCopied)
	}
	res, err = sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{"sh", "-c", "cat /tmp/payload.bin && stat -c %a /tmp/payload.bin"}})
	if err != nil {
		t.Fatalf("Exec readback: %v", err)
	}
	if !strings.Contains(string(res.Stdout), "payload-bytes") || !strings.Contains(string(res.Stdout), "640") {
		t.Errorf("readback = %q, want content and mode 640", res.Stdout)
	}

	if err := sbx.Destroy(ctx); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if err := sbx.Destroy(ctx); err != nil {
		t.Fatalf("second Destroy must be idempotent: %v", err)
	}
	destroyed = true
}

// uniqueHostID returns a host id in the same shape as sandbox.HostID() that
// no other process on this machine can derive, so a host-scoped sweep in
// one test cannot collide with a concurrent drill's.
func uniqueHostID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random host id: %v", err)
	}
	return hex.EncodeToString(b)
}

// TestSweepOrphansReal verifies the sweep removes dead-owner containers and
// spares live-owner ones, against a real daemon.
func TestSweepOrphansReal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	p := New(nil)
	// Scope this test to a host id nothing else shares. A sweep is
	// host-scoped, not process-scoped — that is the product behaviour, and
	// it is correct: an orphan belongs to whoever finds it. But every
	// probavi process on one machine derives the same host id from the
	// hostname, and every drill sweeps at startup (internal/core), so a
	// drill running in another test package against this same daemon reaps
	// the orphan planted below and leaves this sweep with nothing to find.
	// That is what turned this test red on main while the identical code
	// passed twice on its branch. A unique id keeps every assertion below
	// and removes the race.
	p.hostID = uniqueHostID(t)

	// A live sandbox owned by this test process.
	live, err := p.Create(ctx, map[string]string{"image": testImage})
	if err != nil {
		t.Fatalf("Create live sandbox: %v", err)
	}
	defer func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dcancel()
		_ = live.Destroy(dctx)
	}()

	// An orphan: our host label, owner pid that cannot exist.
	out, err := exec.CommandContext(ctx, "docker", "run", "-d",
		"--label", LabelSandbox+"=1", "--label", labelPID+"=2147483646",
		"--label", labelHost+"="+p.hostID,
		"--network", "none", testImage, "sleep", "60").Output()
	if err != nil {
		t.Fatalf("start orphan container: %v", err)
	}
	orphanID := strings.TrimSpace(string(out))
	defer exec.Command("docker", "rm", "-f", "-v", orphanID).Run() //nolint:errcheck // best-effort cleanup if the sweep failed

	// A foreign drill host's sandbox on the same daemon: its dead pid means
	// nothing here, the sweep must leave it alone.
	out, err = exec.CommandContext(ctx, "docker", "run", "-d",
		"--label", LabelSandbox+"=1", "--label", labelPID+"=2147483646",
		"--label", labelHost+"=ffff0000ffff0000",
		"--network", "none", testImage, "sleep", "60").Output()
	if err != nil {
		t.Fatalf("start foreign container: %v", err)
	}
	foreignID := strings.TrimSpace(string(out))
	defer exec.Command("docker", "rm", "-f", "-v", foreignID).Run() //nolint:errcheck // the foreign container is always ours to clean up

	removed, err := p.SweepOrphans(ctx)
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	removedOrphan := false
	for _, id := range removed {
		if strings.HasPrefix(orphanID, id) || strings.HasPrefix(id, orphanID) {
			removedOrphan = true
		}
		if strings.HasPrefix(live.ID(), id) || strings.HasPrefix(id, live.ID()) {
			t.Errorf("sweep removed the live sandbox %s", live.ID())
		}
		if strings.HasPrefix(foreignID, id) || strings.HasPrefix(id, foreignID) {
			t.Errorf("sweep removed another host's sandbox %s", foreignID)
		}
	}
	if !removedOrphan {
		t.Errorf("sweep did not remove the orphan %s (removed: %v)", orphanID, removed)
	}
	if err := exec.CommandContext(ctx, "docker", "inspect", foreignID).Run(); err != nil {
		t.Errorf("foreign-host sandbox is gone after the sweep: %v", err)
	}
}

// TestRemoteDockerOverSSH proves the provider works unchanged against a
// remote daemon reached through the docker CLI's native SSH transport —
// the "remote host via SSH" deployment. CI loops the connection back to
// the runner itself; the provider cannot tell the difference. Requires
// key-based ssh-to-self and is skipped unless PROBAVI_IT_SSH=1.
func TestRemoteDockerOverSSH(t *testing.T) {
	if os.Getenv("PROBAVI_IT_SSH") != "1" {
		t.Skip("set PROBAVI_IT_SSH=1 (with key-based ssh to the target configured) to run the remote-docker suite")
	}
	endpoint := os.Getenv("PROBAVI_IT_SSH_HOST")
	if endpoint == "" {
		endpoint = "ssh://127.0.0.1"
	}
	t.Setenv("DOCKER_HOST", endpoint)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	p := New(nil)

	sbx, err := p.Create(ctx, map[string]string{"image": testImage})
	if err != nil {
		t.Fatalf("Create over %s: %v", endpoint, err)
	}
	defer func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dcancel()
		_ = sbx.Destroy(dctx)
	}()

	// Exec streams stdin through the client connection.
	res, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{"cat"}, Stdin: []byte("over-ssh")})
	if err != nil {
		t.Fatalf("Exec over ssh: %v", err)
	}
	if res.ExitCode != 0 || string(res.Stdout) != "over-ssh" {
		t.Errorf("stdin roundtrip: exit=%d out=%q", res.ExitCode, res.Stdout)
	}

	// PutFile is the interesting verb remotely: docker cp must stream the
	// local backup bytes through the SSH connection, never a published port.
	hostFile := filepath.Join(t.TempDir(), "backup.bin")
	if err := os.WriteFile(hostFile, []byte("backup-bytes"), 0o600); err != nil {
		t.Fatalf("write host file: %v", err)
	}
	if _, err := sbx.PutFile(ctx, hostFile, sbx.ScratchDir()+"/backup.bin", "0600"); err != nil {
		t.Fatalf("PutFile over ssh: %v", err)
	}
	res, err = sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{"cat", "/tmp/backup.bin"}})
	if err != nil || string(res.Stdout) != "backup-bytes" {
		t.Errorf("readback = %q err=%v", res.Stdout, err)
	}

	// The sweep runs against the remote daemon and must stay host-scoped.
	if _, err := p.SweepOrphans(ctx); err != nil {
		t.Fatalf("SweepOrphans over ssh: %v", err)
	}
	if err := exec.CommandContext(ctx, "docker", "inspect", sbx.ID()).Run(); err != nil {
		t.Errorf("live sandbox vanished after remote sweep: %v", err)
	}

	if err := sbx.Destroy(ctx); err != nil {
		t.Fatalf("Destroy over ssh: %v", err)
	}
	if err := sbx.Destroy(ctx); err != nil {
		t.Fatalf("second Destroy must stay idempotent remotely: %v", err)
	}
}
