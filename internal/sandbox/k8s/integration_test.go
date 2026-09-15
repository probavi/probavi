//go:build integration

package k8s

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/probavi/probavi/internal/adapter"
	"github.com/probavi/probavi/internal/sandbox"
)

// itImage is used for every integration sandbox: with the default
// entrypoint it boots a real engine (the end-to-end drill), with
// `command: sleep infinity` it is an idle pod with a full shell.
const itImage = "postgres:16"

// requireCluster skips when no Kubernetes cluster is reachable — unless
// PROBAVI_IT_K8S is set (CI), where missing infrastructure must fail loud,
// never silently skip the axis the job exists to test.
func requireCluster(t *testing.T) {
	t.Helper()
	out, err := exec.Command("kubectl", "cluster-info").CombinedOutput()
	if err == nil {
		return
	}
	if os.Getenv("PROBAVI_IT_K8S") != "" {
		t.Fatalf("PROBAVI_IT_K8S is set but no cluster answers: %v: %s", err, out)
	}
	t.Skipf("no reachable Kubernetes cluster (create one with kind to run this): %v", err)
}

func idleParams() map[string]string {
	return map[string]string{"image": itImage, "command": "sleep infinity", "env.PROBAVI_TEST": "1"}
}

// TestK8sLifecycle drives a real pod through the full provider contract:
// create, exec (plain, stdin, exit codes, container env, per-exec env),
// put_file, security defaults, idempotent destroy with nothing left behind.
func TestK8sLifecycle(t *testing.T) {
	requireCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	p := New(nil)

	sbx, err := p.Create(ctx, idleParams())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	destroyed := false
	defer func() {
		if !destroyed {
			dctx, dcancel := context.WithTimeout(context.Background(), 60*time.Second)
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
		t.Errorf("env param did not reach the pod: out=%q err=%v", res.Stdout, err)
	}

	res, err = sbx.Exec(ctx, sandbox.ExecRequest{
		Argv: []string{"printenv", "PROBAVI_EXEC_ENV"},
		Env:  map[string]string{"PROBAVI_EXEC_ENV": "yes"},
	})
	if err != nil || strings.TrimSpace(string(res.Stdout)) != "yes" {
		t.Errorf("per-exec env did not apply: out=%q err=%v", res.Stdout, err)
	}

	// Security default: the pod must not hold cluster credentials.
	res, err = sbx.Exec(ctx, sandbox.ExecRequest{
		Argv: []string{"sh", "-c", "test ! -e /var/run/secrets/kubernetes.io/serviceaccount/token"}})
	if err != nil {
		t.Fatalf("Exec token check: %v", err)
	}
	if res.ExitCode != 0 {
		t.Error("a service-account token is mounted — a sandbox holding production data must carry no cluster credentials")
	}

	// A payload large enough to cross the buffer boundaries a torn exec
	// stream stops at: the 13-byte fixture this test used until issue #272
	// fitted inside a single frame and could not have caught a truncated
	// transfer. Repeated because the tear-down race does not lose the tail
	// every time — a single green copy proves nothing about the next one.
	const payloadBytes = 512 * 1024
	payload := make([]byte, payloadBytes)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("generate payload: %v", err)
	}
	wantDigest := fmt.Sprintf("%x", sha256.Sum256(payload))
	hostFile := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(hostFile, payload, 0o600); err != nil {
		t.Fatalf("write host file: %v", err)
	}
	for i := 1; i <= 3; i++ {
		pf, err := sbx.PutFile(ctx, hostFile, sbx.ScratchDir()+"/payload.bin", "0640")
		if err != nil {
			t.Fatalf("PutFile transfer %d: %v", i, err)
		}
		if pf.BytesCopied != payloadBytes {
			t.Errorf("transfer %d: BytesCopied = %d, want %d", i, pf.BytesCopied, payloadBytes)
		}
		res, err = sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{"sh", "-c",
			"wc -c < /tmp/payload.bin && sha256sum /tmp/payload.bin | cut -d' ' -f1 && stat -c %a /tmp/payload.bin"}})
		if err != nil {
			t.Fatalf("transfer %d: Exec readback: %v", i, err)
		}
		fields := strings.Fields(string(res.Stdout))
		want := []string{strconv.Itoa(payloadBytes), wantDigest, "640"}
		if !slices.Equal(fields, want) {
			t.Errorf("transfer %d: readback = %v, want size, digest and mode %v", i, fields, want)
		}
	}

	if err := sbx.Destroy(ctx); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if err := sbx.Destroy(ctx); err != nil {
		t.Fatalf("second Destroy must be idempotent: %v", err)
	}
	destroyed = true

	out, err := exec.CommandContext(ctx, "kubectl", "get", "job", "-n", sbx.namespace, sbx.job).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "not found") {
		t.Errorf("job still exists after Destroy: %v: %s", err, out)
	}
}

// TestSweepOrphansReal verifies against a real cluster that the sweep
// removes this host's dead-owner Jobs, spares live ones, and never touches
// another host's sandboxes.
func TestSweepOrphansReal(t *testing.T) {
	requireCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	p := New(nil)

	live, err := p.Create(ctx, idleParams())
	if err != nil {
		t.Fatalf("Create live sandbox: %v", err)
	}
	defer destroyQuietly(live)

	orphanOwner := New(nil)
	orphanOwner.pid = 2147483646 // near pid_max: no live process holds it
	orphan, err := orphanOwner.Create(ctx, idleParams())
	if err != nil {
		t.Fatalf("Create orphan sandbox: %v", err)
	}
	defer destroyQuietly(orphan)

	foreignOwner := New(nil)
	foreignOwner.pid = 2147483646
	foreignOwner.hostID = "ffffffffffffffff"
	foreign, err := foreignOwner.Create(ctx, idleParams())
	if err != nil {
		t.Fatalf("Create foreign sandbox: %v", err)
	}
	defer destroyQuietly(foreign)

	removed, err := p.SweepOrphans(ctx)
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	has := func(id string) bool {
		for _, r := range removed {
			if r == id {
				return true
			}
		}
		return false
	}
	if !has(orphan.ID()) {
		t.Errorf("sweep did not remove the dead-owner job %s (removed: %v)", orphan.ID(), removed)
	}
	if has(live.ID()) {
		t.Errorf("sweep removed the live sandbox %s", live.ID())
	}
	if has(foreign.ID()) {
		t.Errorf("sweep removed another host's sandbox %s — liveness is not checkable across hosts", foreign.ID())
	}
}

// TestEndToEndRestoreDrillOnK8s is the second-axis proof (ROADMAP Phase 2):
// the unchanged postgres adapter restores a real pg_dump into a Kubernetes
// pod through the same core-mediated verbs it uses with docker — the
// provider swapped, nothing else.
func TestEndToEndRestoreDrillOnK8s(t *testing.T) {
	requireCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	binDir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o",
		filepath.Join(binDir, "probavi-adapter-postgres"), "../../../adapters/postgres").CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	p := New(nil)
	engineParams := map[string]string{"image": itImage, "env.POSTGRES_HOST_AUTH_METHOD": "trust"}

	fixture := filepath.Join(t.TempDir(), "orders.dump")
	makeFixture(t, ctx, p, engineParams, fixture)

	sbx, err := p.Create(ctx, engineParams)
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroyQuietly(sbx)

	runner, err := adapter.New("postgres", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "pgdump", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if res.Timings.RestoreSeconds <= 0 || res.Timings.EngineReadySeconds <= 0 {
		t.Errorf("timings = %+v, want real measurements", res.Timings)
	}

	health, err := runner.Healthcheck(ctx, &res.Connection, res.State, sbx)
	if err != nil || !health.Healthy {
		t.Fatalf("healthcheck = %+v err=%v", health, err)
	}
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{
		"psql", "-h", "127.0.0.1", "-U", "postgres", "-d", "postgres", "-tA", "-c",
		"SELECT count(*) FROM orders"}})
	if err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count := strings.TrimSpace(string(out.Stdout)); out.ExitCode != 0 || count != "300" {
		t.Fatalf("row count = %q (exit %d, stderr %s), want 300", count, out.ExitCode, out.Stderr)
	}
	if _, err := runner.Teardown(ctx, res.State, "completed", sbx); err != nil {
		t.Fatalf("teardown: %v", err)
	}
}

// makeFixture seeds a real engine in its own pod and pulls a pg_dump out.
// The provider deliberately has no get-file verb; extracting the fixture is
// test-harness work, done with kubectl cp (the postgres image carries tar).
func makeFixture(t *testing.T, ctx context.Context, p *Provider, params map[string]string, dest string) {
	t.Helper()
	seed, err := p.Create(ctx, params)
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroyQuietly(seed)

	awaitEngine(t, ctx, seed)
	seedSQL := `CREATE TABLE orders (id bigserial PRIMARY KEY, total numeric(10,2) NOT NULL);
INSERT INTO orders (total) SELECT (random()*100)::numeric(10,2) FROM generate_series(1,300);`
	mustExec(t, ctx, seed, "psql", "-h", "127.0.0.1", "-U", "postgres", "-v", "ON_ERROR_STOP=1", "-c", seedSQL)
	mustExec(t, ctx, seed, "pg_dump", "-h", "127.0.0.1", "-U", "postgres", "-Fc", "-f", "/tmp/fixture.dump", "postgres")

	if out, err := exec.CommandContext(ctx, "kubectl", "cp",
		seed.namespace+"/"+seed.pod+":/tmp/fixture.dump", dest).CombinedOutput(); err != nil {
		t.Fatalf("extract fixture: %v: %s", err, out)
	}
}

func awaitEngine(t *testing.T, ctx context.Context, sbx *Sandbox) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		res, err := sbx.Exec(ctx, sandbox.ExecRequest{
			Argv: []string{"pg_isready", "-h", "127.0.0.1", "-U", "postgres", "-q"}, Timeout: 5 * time.Second,
		})
		if err == nil && res.ExitCode == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("seed engine never became ready")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func mustExec(t *testing.T, ctx context.Context, sbx *Sandbox, argv ...string) {
	t.Helper()
	res, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: argv})
	if err != nil {
		t.Fatalf("exec %v: %v", argv, err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exec %v: exit %d: %s", argv, res.ExitCode, res.Stderr)
	}
}

func destroyQuietly(sbx *Sandbox) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_ = sbx.Destroy(ctx)
}
