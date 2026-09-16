//go:build integration

package remotehost

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/probavi/probavi/internal/sandbox"
)

// TestRemoteHostLifecycle drives a real target through the full provider
// contract: systemd probe, create (slice + workspace + caps + backstop
// timer), exec inside the slice as the drill user, put_file, host-scoped
// sweep, idempotent destroy. It requires key-based ssh to 127.0.0.1 with
// the polkit rule from the README installed for the connecting user (the
// CI integration job provisions exactly that), and it asserts through the
// local filesystem and systemctl — the loopback IS the target. Skipped
// unless PROBAVI_IT_REMOTEHOST=1; once that is set, failures are failures.
func TestRemoteHostLifecycle(t *testing.T) {
	if os.Getenv("PROBAVI_IT_REMOTEHOST") != "1" {
		t.Skip("set PROBAVI_IT_REMOTEHOST=1 (loopback ssh + polkit rule configured) to run the bare-host suite")
	}
	root := os.Getenv("PROBAVI_IT_REMOTEHOST_ROOT")
	if root == "" {
		root = defaultWorkspaceRoot
	}
	t.Setenv(EnvTarget, "127.0.0.1")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	params := map[string]string{"workspace_root": root, "memory": "256M", "cpus": "1"}
	p, err := New(nil, params)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Plant one dead-owner and one foreign-host workspace directly on the
	// (loopback) filesystem: the sweep must reap the first and never touch
	// the second.
	deadName := namePrefix + "it-dead"
	foreignName := namePrefix + "it-foreign"
	for name, marker := range map[string]string{
		deadName:    sandbox.HostID() + " 999999999",
		foreignName: "ffff0000ffff0000 999999999",
	} {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("plant %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "owner"), []byte(marker+"\n"), 0o600); err != nil {
			t.Fatalf("plant %s marker: %v", name, err)
		}
	}
	defer os.RemoveAll(filepath.Join(root, foreignName)) //nolint:errcheck // best-effort test cleanup

	sbx, err := p.Create(ctx, params)
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
	workspace := filepath.Join(root, sbx.ID())

	// The workspace, scratch dir, and owner marker exist with the promised
	// shapes.
	if info, err := os.Stat(workspace); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("workspace = %v, %v — want a 0700 directory", info, err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "scratch")); err != nil {
		t.Errorf("scratch dir: %v", err)
	}
	marker, err := os.ReadFile(filepath.Join(workspace, "owner"))
	if err != nil || !strings.HasPrefix(string(marker), sandbox.HostID()+" ") {
		t.Errorf("owner marker = %q, %v — want this host's id", marker, err)
	}

	// The slice carries the MemoryMax cap and the deadline timer is armed.
	if out, err := exec.Command("systemctl", "show", "-p", "MemoryMax", sbx.ID()+".slice").Output(); err != nil || !strings.Contains(string(out), "MemoryMax=268435456") {
		t.Errorf("slice MemoryMax = %q, %v — want the 256M cap applied", out, err)
	}
	if err := exec.Command("systemctl", "is-active", "--quiet", sbx.ID()+"-reaper.timer").Run(); err != nil {
		t.Errorf("deadline backstop timer is not armed: %v", err)
	}

	// Exec: stdin roundtrip, environment, working directory, user, exit
	// codes — all inside the slice.
	res, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{"cat"}, Stdin: []byte("piped-data")})
	if err != nil {
		t.Fatalf("Exec cat: %v", err)
	}
	if res.ExitCode != 0 || string(res.Stdout) != "piped-data" {
		t.Errorf("stdin roundtrip: exit=%d out=%q", res.ExitCode, res.Stdout)
	}
	res, err = sbx.Exec(ctx, sandbox.ExecRequest{
		Argv: []string{"sh", "-c", `printf '%s|%s|%s' "$PROBAVI_MARK" "$(pwd)" "$(id -un)"`},
		Env:  map[string]string{"PROBAVI_MARK": "it's alive"},
	})
	if err != nil {
		t.Fatalf("Exec env/cwd/user: %v", err)
	}
	wantIdentity := "it's alive|" + workspace + "|" + currentUser(t)
	if string(res.Stdout) != wantIdentity {
		t.Errorf("env/cwd/user = %q, want %q", res.Stdout, wantIdentity)
	}
	res, err = sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{"sh", "-c", "exit 7"}})
	if err != nil {
		t.Fatalf("Exec exit 7: %v", err)
	}
	if res.ExitCode != 7 {
		t.Errorf("exit code = %d, want 7 (non-zero exits are results, not errors)", res.ExitCode)
	}

	// An engine started by one exec is still there for the next (issue
	// #292). The next exec asking is the adapter's view — a readiness poll —
	// and the cgroup is the sandbox's: the survivor must be inside the slice.
	leftover := startLeftover(t, ctx, sbx)

	// PutFile: bytes land under scratch with the default 0600 mode.
	hostFile := filepath.Join(t.TempDir(), "backup.dump")
	if err := os.WriteFile(hostFile, []byte("0123456789"), 0o600); err != nil {
		t.Fatalf("write host file: %v", err)
	}
	dest := sbx.ScratchDir() + "/backup.dump"
	pres, err := sbx.PutFile(ctx, hostFile, dest, "")
	if err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	if pres.BytesCopied != 10 {
		t.Errorf("BytesCopied = %d, want 10", pres.BytesCopied)
	}
	if data, err := os.ReadFile(dest); err != nil || string(data) != "0123456789" {
		t.Errorf("dest content = %q, %v", data, err)
	}
	if info, err := os.Stat(dest); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("dest mode = %v, %v — want 0600", info, err)
	}

	// Host-scoped sweep: the planted dead workspace goes, the foreign one
	// and the live sandbox survive.
	removed, err := p.SweepOrphans(ctx)
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if !slices.Contains(removed, deadName) {
		t.Errorf("removed = %v, want it to include the dead-owner workspace %s", removed, deadName)
	}
	if slices.Contains(removed, foreignName) || slices.Contains(removed, sbx.ID()) {
		t.Errorf("removed = %v — foreign-host and live sandboxes must survive the sweep", removed)
	}
	if _, err := os.Stat(filepath.Join(root, deadName)); !os.IsNotExist(err) {
		t.Errorf("dead workspace still present: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, foreignName)); err != nil {
		t.Errorf("foreign workspace was touched: %v", err)
	}

	// Destroy: workspace gone, slice stopped, timer disarmed; a second
	// destroy is a no-op success.
	if err := sbx.Destroy(ctx); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	destroyed = true
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Errorf("workspace survived destroy: %v", err)
	}
	if err := exec.Command("systemctl", "is-active", "--quiet", sbx.ID()+".slice").Run(); err == nil {
		t.Error("slice is still active after destroy")
	}
	if err := exec.Command("systemctl", "is-active", "--quiet", sbx.ID()+"-reaper.timer").Run(); err == nil {
		t.Error("deadline backstop timer is still armed after destroy")
	}
	// Stopping the slice alone leaves such a process running; Destroy
	// must not.
	if _, err := os.Stat("/proc/" + leftover); !os.IsNotExist(err) {
		t.Errorf("process %s an exec left running survived destroy: %v", leftover, err)
	}
	if err := sbx.Destroy(ctx); err != nil {
		t.Errorf("second Destroy must succeed (idempotent), got: %v", err)
	}
}

// startLeftover runs an exec that leaves a process behind, the way an
// adapter starts its engine, and returns that process's pid once a second
// exec has found it alive inside the sandbox's slice.
func startLeftover(t *testing.T, ctx context.Context, sbx *Sandbox) string {
	t.Helper()
	res, err := sbx.Exec(ctx, sandbox.ExecRequest{
		Argv: []string{"sh", "-c", `sleep 600 </dev/null >/dev/null 2>&1 & echo $!`},
	})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("Exec starting a background process: %+v, %v", res, err)
	}
	pid := strings.TrimSpace(string(res.Stdout))
	res, err = sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{"kill", "-0", pid}})
	if err != nil {
		t.Fatalf("Exec kill -0: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("process %s ended with the exec that started it — an engine could not outlive its start", pid)
	}
	cgroup, err := os.ReadFile("/proc/" + pid + "/cgroup")
	if err == nil && !strings.Contains(string(cgroup), "/"+sbx.ID()+".slice/") {
		t.Errorf("process %s runs in %q — want it inside %s.slice, under the sandbox's caps", pid, cgroup, sbx.ID())
	}
	return pid
}

func currentUser(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("id", "-un").Output()
	if err != nil {
		t.Fatalf("id -un: %v", err)
	}
	return strings.TrimSpace(string(out))
}
