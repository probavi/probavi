package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/probavi/probavi/internal/sandbox"
)

// deadPID is close to the default pid_max; no live process realistically
// holds it during a test run.
const deadPID = 2147483646

type response struct {
	stdout    string
	stderr    string
	truncated bool
	exit      int
	err       error
}

// fakeRunner scripts subprocess responses and records every invocation.
type fakeRunner struct {
	envs      [][]string
	t         *testing.T
	calls     [][]string
	stdins    []string
	responses []response
}

func (f *fakeRunner) Run(_ context.Context, stdin io.Reader, env []string, name string, args ...string) ([]byte, []byte, bool, int, error) {
	f.envs = append(f.envs, env)
	f.t.Helper()
	in := ""
	if stdin != nil {
		b, err := io.ReadAll(stdin)
		if err != nil {
			f.t.Fatalf("read stdin: %v", err)
		}
		in = string(b)
	}
	f.calls = append(f.calls, append([]string{name}, args...))
	f.stdins = append(f.stdins, in)
	if len(f.responses) == 0 {
		f.t.Fatalf("unexpected call: %s %v", name, args)
	}
	r := f.responses[0]
	f.responses = f.responses[1:]
	return []byte(r.stdout), []byte(r.stderr), r.truncated, r.exit, r.err
}

func testProvider(t *testing.T, responses ...response) (*Provider, *fakeRunner) {
	t.Helper()
	fake := &fakeRunner{t: t, responses: responses}
	return &Provider{
		bin:           "kubectl",
		run:           fake,
		logger:        slog.New(slog.DiscardHandler),
		pid:           os.Getpid(),
		hostID:        "abcd1234abcd1234",
		awaitInterval: time.Millisecond,
		awaitCap:      50 * time.Millisecond,
		alive:         sandbox.OwnerAlive,
	}, fake
}

func runningPodJSON(name string) string {
	return fmt.Sprintf(`{"items":[{"metadata":{"name":%q},"status":{"phase":"Running"}}]}`, name)
}

func TestNewDefaults(t *testing.T) {
	p := New(nil)
	if p.bin != "kubectl" || p.run == nil || p.logger == nil {
		t.Errorf("New: %+v, want kubectl binary with runner and logger", p)
	}
	if p.alive == nil {
		t.Error("New: alive is nil — the orphan sweep would panic on its first labeled Job")
	}
	if len(p.hostID) != 16 {
		t.Errorf("hostID = %q, want 16 hex chars", p.hostID)
	}
}

// fullManifest builds a manifest from every supported param.
func fullManifest(t *testing.T) (*jobManifest, *Provider) {
	t.Helper()
	p, _ := testProvider(t)
	m, namespace, err := p.manifest(Descriptor, map[string]string{
		"image":     "postgres:16",
		"namespace": "drills",
		"memory":    "2Gi",
		"cpus":      "2",
		"command":   "sleep  infinity",
		"env.B_VAR": "2",
		"env.A_VAR": "1",
	})
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if namespace != "drills" {
		t.Errorf("namespace = %q, want drills", namespace)
	}
	return m, p
}

func TestManifestJobShape(t *testing.T) {
	m, p := fullManifest(t)
	if m.APIVersion != "batch/v1" || m.Kind != "Job" {
		t.Errorf("object = %s/%s, want batch/v1 Job", m.APIVersion, m.Kind)
	}
	if !strings.HasPrefix(m.Metadata.Name, "probavi-sbx-") {
		t.Errorf("name = %q", m.Metadata.Name)
	}
	for _, labels := range []map[string]string{m.Metadata.Labels, m.Spec.Template.Metadata.Labels} {
		if labels[LabelSandbox] != "1" || labels[labelHost] != p.hostID || labels[labelPID] != sandbox.OwnerID(p.pid) {
			t.Errorf("labels = %v, want sandbox, host, and pid on job and pod", labels)
		}
	}
	if m.Spec.BackoffLimit != 0 || m.Spec.ActiveDeadlineSeconds != activeDeadlineSeconds || m.Spec.TTLSecondsAfterFinished != ttlSecondsAfterFinished {
		t.Errorf("job spec = %+v, want backoff 0 with cleanup deadlines", m.Spec)
	}
}

func TestManifestPodShape(t *testing.T) {
	m, _ := fullManifest(t)
	pod := m.Spec.Template.Spec
	if pod.RestartPolicy != "Never" || pod.AutomountServiceAccountToken || pod.EnableServiceLinks {
		t.Errorf("pod spec = %+v, want Never restart and no token/service links", pod)
	}
	if pod.SecurityContext.SeccompProfile.Type != "RuntimeDefault" {
		t.Errorf("seccomp = %+v", pod.SecurityContext)
	}
	c := pod.Containers[0]
	if c.Image != "postgres:16" || !slices.Equal(c.Command, []string{"sleep", "infinity"}) {
		t.Errorf("container = %+v, want image and whitespace-split command", c)
	}
	if !slices.Equal(c.Env, []envVar{{Name: "A_VAR", Value: "1"}, {Name: "B_VAR", Value: "2"}}) {
		t.Errorf("env = %v, want sorted variables", c.Env)
	}
	want := map[string]string{"memory": "2Gi", "cpu": "2"}
	if c.Resources == nil || fmt.Sprint(c.Resources.Limits) != fmt.Sprint(want) || fmt.Sprint(c.Resources.Requests) != fmt.Sprint(want) {
		t.Errorf("resources = %+v, want requests == limits %v", c.Resources, want)
	}
}

func TestManifestMinimal(t *testing.T) {
	p, _ := testProvider(t)
	m, namespace, err := p.manifest(Descriptor, map[string]string{"image": "x:1"})
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if namespace != "default" {
		t.Errorf("namespace = %q, want default", namespace)
	}
	c := m.Spec.Template.Spec.Containers[0]
	if len(c.Command) != 0 || len(c.Env) != 0 || c.Resources != nil {
		t.Errorf("container = %+v, want image entrypoint, no env, no resources", c)
	}
}

func TestManifestRejects(t *testing.T) {
	p, _ := testProvider(t)
	for name, params := range map[string]map[string]string{
		"missing image": {"namespace": "x"},
		"unknown param": {"image": "x:1", "network": "none"},
		"bad env name":  {"image": "x:1", "env.1BAD": "v"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := p.manifest(Descriptor, params); !errors.Is(err, sandbox.ErrInvalidParams) {
				t.Errorf("manifest(%v): %v, want ErrInvalidParams", params, err)
			}
		})
	}
}

func TestCreateHappyPath(t *testing.T) {
	p, fake := testProvider(t,
		response{stdout: "job.batch/created"},
		response{stdout: `{"items":[]}`},
		response{stdout: runningPodJSON("probavi-sbx-x-abc")},
	)
	sbx, err := p.Create(context.Background(), map[string]string{"image": "x:1", "namespace": "drills"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sbx.pod != "probavi-sbx-x-abc" || sbx.namespace != "drills" {
		t.Errorf("sandbox = %+v", sbx)
	}
	if !strings.HasPrefix(sbx.ID(), "drills/probavi-sbx-") {
		t.Errorf("ID = %q, want namespace/name", sbx.ID())
	}
	if sbx.ScratchDir() != "/tmp" {
		t.Errorf("ScratchDir = %q", sbx.ScratchDir())
	}

	create := fake.calls[0]
	if !slices.Equal(create[:6], []string{"kubectl", "create", "-n", "drills", "-f", "-"}) {
		t.Errorf("create call = %v", create)
	}
	m := jobManifest{}
	if err := json.Unmarshal([]byte(fake.stdins[0]), &m); err != nil {
		t.Fatalf("manifest on stdin is not JSON: %v", err)
	}
	if m.Metadata.Name != strings.TrimPrefix(sbx.ID(), "drills/") {
		t.Errorf("manifest name %q vs sandbox %q", m.Metadata.Name, sbx.ID())
	}
	await := fake.calls[1]
	if !slices.Contains(await, "job-name="+m.Metadata.Name) {
		t.Errorf("await call = %v, want job-name selector", await)
	}
}

func TestCreateFailures(t *testing.T) {
	params := map[string]string{"image": "x:1"}

	t.Run("kubectl create fails", func(t *testing.T) {
		p, _ := testProvider(t, response{exit: 1, stderr: "error: namespace not found"})
		if _, err := p.Create(context.Background(), params); err == nil || !strings.Contains(err.Error(), "namespace not found") {
			t.Errorf("Create: %v, want the kubectl error surfaced", err)
		}
	})

	t.Run("create runner error", func(t *testing.T) {
		p, _ := testProvider(t, response{err: errors.New("kubectl not on PATH")})
		if _, err := p.Create(context.Background(), params); err == nil {
			t.Error("Create must surface runner errors")
		}
	})

	t.Run("pod fails before running", func(t *testing.T) {
		p, fake := testProvider(t,
			response{stdout: "job.batch/created"},
			response{stdout: `{"items":[{"metadata":{"name":"pod-1"},"status":{"phase":"Failed"}}]}`},
			response{}, // delete during cleanup
		)
		_, err := p.Create(context.Background(), params)
		if err == nil || !strings.Contains(err.Error(), "phase Failed") {
			t.Fatalf("Create: %v, want pod-failure error", err)
		}
		last := fake.calls[len(fake.calls)-1]
		if last[1] != "delete" {
			t.Errorf("cleanup call = %v, want kubectl delete", last)
		}
	})

	t.Run("await timeout reports last waiting reason", func(t *testing.T) {
		pending := response{stdout: `{"items":[{"metadata":{"name":"pod-1"},"status":{"phase":"Pending","containerStatuses":[{"state":{"waiting":{"reason":"ImagePullBackOff"}}}]}}]}`}
		p, fake := testProvider(t, append([]response{{stdout: "job.batch/created"}},
			pending, pending, pending, pending, pending, pending, pending, pending, pending, pending,
			pending, pending, pending, pending, pending, pending, pending, pending, pending, pending,
			pending, pending, pending, pending, pending, pending, pending, pending, pending, pending,
			pending, pending, pending, pending, pending, pending, pending, pending, pending, pending,
			pending, pending, pending, pending, pending, pending, pending, pending, pending, pending,
			pending, pending, pending, pending, pending, pending, pending, pending, pending, pending,
			response{}, // delete during cleanup
		)...)
		_, err := p.Create(context.Background(), params)
		if err == nil || !strings.Contains(err.Error(), "ImagePullBackOff") {
			t.Fatalf("Create: %v, want the waiting reason in the error", err)
		}
		if fake.calls[len(fake.calls)-1][1] != "delete" {
			t.Error("timeout must still destroy the job")
		}
	})

}

func TestCreateAwaitQueryFailures(t *testing.T) {
	params := map[string]string{"image": "x:1"}

	t.Run("pod query fails", func(t *testing.T) {
		p, _ := testProvider(t,
			response{stdout: "job.batch/created"},
			response{exit: 1, stderr: "Error from server (Forbidden): pods is forbidden"},
			response{}, // delete during cleanup
		)
		if _, err := p.Create(context.Background(), params); err == nil || !strings.Contains(err.Error(), "kubectl get pods exited") {
			t.Errorf("Create: %v, want the kubectl failure surfaced", err)
		}
	})

	t.Run("pod list is not JSON", func(t *testing.T) {
		p, _ := testProvider(t,
			response{stdout: "job.batch/created"},
			response{stdout: "not json"},
			response{}, // delete during cleanup
		)
		if _, err := p.Create(context.Background(), params); err == nil || !strings.Contains(err.Error(), "parse pod list") {
			t.Errorf("Create: %v, want parse error", err)
		}
	})
}

func TestFirstLine(t *testing.T) {
	if got := firstLine([]byte("  first\nsecond")); got != "first" {
		t.Errorf("firstLine = %q", got)
	}
	if got := firstLine([]byte("only")); got != "only" {
		t.Errorf("firstLine = %q", got)
	}
}

func testSandbox(t *testing.T, responses ...response) (*Sandbox, *fakeRunner) {
	t.Helper()
	p, fake := testProvider(t, responses...)
	return &Sandbox{job: "probavi-sbx-1", pod: "probavi-sbx-1-abc", namespace: "drills", p: p}, fake
}

func TestExec(t *testing.T) {
	t.Run("argv, env, and stdin", func(t *testing.T) {
		sbx, fake := testSandbox(t, response{stdout: "out", stderr: "err", exit: 3})
		res, err := sbx.Exec(context.Background(), sandbox.ExecRequest{
			Argv:  []string{"psql", "-c", "SELECT 1"},
			Env:   map[string]string{"B": "2", "A": "1"},
			Stdin: []byte("piped"),
		})
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if res.ExitCode != 3 || string(res.Stdout) != "out" || string(res.Stderr) != "err" {
			t.Errorf("result = %+v", res)
		}
		want := []string{"kubectl", "exec", "-n", "drills", "-i", "probavi-sbx-1-abc", "--",
			"sh", "-c", sandbox.EnvPreludeScript(2), "sh", "psql", "-c", "SELECT 1"}
		if !slices.Equal(fake.calls[0], want) {
			t.Errorf("call = %v\nwant   %v", fake.calls[0], want)
		}
		// The env block precedes the request's own stdin, which reaches the
		// command untouched once the prelude has consumed exactly two lines.
		if fake.stdins[0] != "A=1\nB=2\npiped" {
			t.Errorf("stdin = %q, want the env block followed by the caller's stdin", fake.stdins[0])
		}
	})

	t.Run("no env and no stdin stays plain", func(t *testing.T) {
		sbx, fake := testSandbox(t, response{})
		if _, err := sbx.Exec(context.Background(), sandbox.ExecRequest{Argv: []string{"true"}}); err != nil {
			t.Fatalf("Exec: %v", err)
		}
		want := []string{"kubectl", "exec", "-n", "drills", "probavi-sbx-1-abc", "--", "true"}
		if !slices.Equal(fake.calls[0], want) {
			t.Errorf("call = %v\nwant   %v", fake.calls[0], want)
		}
	})

	t.Run("timeout is honored", func(t *testing.T) {
		sbx, _ := testSandbox(t)
		sbx.p.run = timeoutRunner{}
		start := time.Now()
		_, err := sbx.Exec(context.Background(), sandbox.ExecRequest{Argv: []string{"sleep"}, Timeout: 10 * time.Millisecond})
		if err == nil || time.Since(start) > 5*time.Second {
			t.Errorf("Exec with timeout: err=%v", err)
		}
	})

	t.Run("runner error surfaces", func(t *testing.T) {
		sbx, _ := testSandbox(t, response{err: errors.New("spawn failed")})
		if _, err := sbx.Exec(context.Background(), sandbox.ExecRequest{Argv: []string{"true"}}); err == nil {
			t.Error("Exec must surface runner errors")
		}
	})
}

// timeoutRunner blocks until the context dies, proving deadlines propagate.
type timeoutRunner struct{}

func (timeoutRunner) Run(ctx context.Context, _ io.Reader, _ []string, name string, _ ...string) ([]byte, []byte, bool, int, error) {
	<-ctx.Done()
	return nil, nil, false, 0, fmt.Errorf("%s: %w", name, ctx.Err())
}

func writeSource(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "backup.dump")
	if err := os.WriteFile(path, []byte("dump-bytes"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	return path
}

func TestPutFile(t *testing.T) {
	t.Run("streams through cat with positional dest", func(t *testing.T) {
		sbx, fake := testSandbox(t, response{stdout: "10\n"}, response{})
		res, err := sbx.PutFile(context.Background(), writeSource(t), "/tmp/x.dump", "0644")
		if err != nil {
			t.Fatalf("PutFile: %v", err)
		}
		if res.BytesCopied != int64(len("dump-bytes")) {
			t.Errorf("BytesCopied = %d", res.BytesCopied)
		}
		wantCopy := []string{"kubectl", "exec", "-n", "drills", "-i", "probavi-sbx-1-abc", "--",
			"sh", "-c", `cat > "$1" && wc -c < "$1"`, "sh", "/tmp/x.dump"}
		if !slices.Equal(fake.calls[0], wantCopy) {
			t.Errorf("copy call = %v\nwant      %v", fake.calls[0], wantCopy)
		}
		if fake.stdins[0] != "dump-bytes" {
			t.Errorf("stdin = %q, want the file content streamed", fake.stdins[0])
		}
		wantChmod := []string{"kubectl", "exec", "-n", "drills", "probavi-sbx-1-abc", "--", "chmod", "0644", "/tmp/x.dump"}
		if !slices.Equal(fake.calls[1], wantChmod) {
			t.Errorf("chmod call = %v\nwant       %v", fake.calls[1], wantChmod)
		}
	})

	t.Run("default mode is 0600", func(t *testing.T) {
		sbx, fake := testSandbox(t, response{stdout: "10\n"}, response{})
		if _, err := sbx.PutFile(context.Background(), writeSource(t), "/tmp/x", ""); err != nil {
			t.Fatalf("PutFile: %v", err)
		}
		if !slices.Contains(fake.calls[1], "0600") {
			t.Errorf("chmod call = %v, want default 0600", fake.calls[1])
		}
	})

}

// TestPutFileVerifiesTheBytesLanded covers the guarantee the transfer makes
// about itself, which until issue #272 it did not make at all.
func TestPutFileVerifiesTheBytesLanded(t *testing.T) {
	// The count BytesCopied reports is the pod's, not the host file's stat.
	// Reading it from the host is what made a truncated transfer look like a
	// success (issue #272), so a report that cannot tell the two apart is not
	// a report at all: here the pod's number is deliberately the wrong one
	// and the transfer must be refused rather than agreed with.
	t.Run("the count comes from the pod, not the host stat", func(t *testing.T) {
		short := response{stdout: "4\n"}
		sbx, fake := testSandbox(t, short, short, short)
		_, err := sbx.PutFile(context.Background(), writeSource(t), "/tmp/x", "")
		if err == nil {
			t.Fatal("a copy that landed 4 of 10 bytes must not be reported as a success")
		}
		for _, want := range []string{"received 4 of 10 bytes", "not what was sent"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %v, want it to contain %q", err, want)
			}
		}
		if len(fake.calls) != putFileAttempts {
			t.Errorf("made %d calls, want %d attempts before refusing", len(fake.calls), putFileAttempts)
		}
	})

	// A torn exec stream is the transport failing, not the cluster being
	// broken, so the send is repeated — and a later attempt that lands every
	// byte is the successful transfer it is.
	t.Run("a short copy is retried and the full one accepted", func(t *testing.T) {
		sbx, fake := testSandbox(t, response{stdout: "4\n"}, response{stdout: "10\n"}, response{})
		res, err := sbx.PutFile(context.Background(), writeSource(t), "/tmp/x", "")
		if err != nil {
			t.Fatalf("PutFile: %v", err)
		}
		if res.BytesCopied != 10 {
			t.Errorf("BytesCopied = %d, want the 10 the second attempt landed", res.BytesCopied)
		}
		if fake.stdins[1] != "dump-bytes" {
			t.Errorf("retry stdin = %q, want the file re-sent from the beginning", fake.stdins[1])
		}
		if len(fake.calls) != 3 {
			t.Errorf("made %d calls, want copy, retry, chmod", len(fake.calls))
		}
	})

	// An image without wc cannot answer, and an unanswerable transfer is
	// refused rather than taken on trust — the whole point of the check.
	t.Run("a pod that reports no count is an error", func(t *testing.T) {
		sbx, _ := testSandbox(t, response{stdout: "sh: wc: not found\n"})
		_, err := sbx.PutFile(context.Background(), writeSource(t), "/tmp/x", "")
		if err == nil || !strings.Contains(err.Error(), "POSIX wc") {
			t.Errorf("error = %v, want the missing byte count named", err)
		}
	})
}

func TestPutFileFailures(t *testing.T) {
	src := writeSource(t)
	sbx, _ := testSandbox(t)
	if _, err := sbx.PutFile(context.Background(), src, "/tmp/x", "rw-"); !errors.Is(err, sandbox.ErrInvalidParams) {
		t.Errorf("bad mode: %v, want ErrInvalidParams", err)
	}
	if _, err := sbx.PutFile(context.Background(), filepath.Join(t.TempDir(), "gone"), "/tmp/x", ""); err == nil {
		t.Error("missing source must be an error")
	}

	sbx, _ = testSandbox(t, response{exit: 1, stderr: "no space left on device"})
	if _, err := sbx.PutFile(context.Background(), src, "/tmp/x", ""); err == nil || !strings.Contains(err.Error(), "no space") {
		t.Errorf("copy failure: %v", err)
	}
	sbx, _ = testSandbox(t, response{stdout: "10\n"}, response{exit: 1, stderr: "chmod: not permitted"})
	if _, err := sbx.PutFile(context.Background(), src, "/tmp/x", ""); err == nil || !strings.Contains(err.Error(), "chmod") {
		t.Errorf("chmod failure: %v", err)
	}
	sbx, _ = testSandbox(t, response{err: errors.New("spawn failed")})
	if _, err := sbx.PutFile(context.Background(), src, "/tmp/x", ""); err == nil {
		t.Error("runner error must surface")
	}
}

func TestDestroy(t *testing.T) {
	t.Run("deletes with foreground cascade, idempotently", func(t *testing.T) {
		sbx, fake := testSandbox(t, response{})
		if err := sbx.Destroy(context.Background()); err != nil {
			t.Fatalf("Destroy: %v", err)
		}
		call := fake.calls[0]
		for _, want := range []string{"delete", "job", "probavi-sbx-1", "--cascade=foreground", "--ignore-not-found"} {
			if !slices.Contains(call, want) {
				t.Errorf("delete call = %v, missing %q", call, want)
			}
		}
	})
	t.Run("failure surfaces", func(t *testing.T) {
		sbx, _ := testSandbox(t, response{exit: 1, stderr: "forbidden"})
		if err := sbx.Destroy(context.Background()); err == nil || !strings.Contains(err.Error(), "forbidden") {
			t.Errorf("Destroy: %v", err)
		}
		sbx, _ = testSandbox(t, response{err: errors.New("spawn failed")})
		if err := sbx.Destroy(context.Background()); err == nil {
			t.Error("runner error must surface")
		}
	})
}

func sweepJobJSON(name, ns, host, pid string) string {
	return fmt.Sprintf(`{"metadata":{"name":%q,"namespace":%q,"labels":{"com.probavi.sandbox":"1","com.probavi.host":%q,"com.probavi.pid":%q}}}`,
		name, ns, host, pid)
}

func TestSweepOrphans(t *testing.T) {
	t.Run("removes only this host's dead-owner jobs", func(t *testing.T) {
		p, fake := testProvider(t)
		list := `{"items":[` + strings.Join([]string{
			sweepJobJSON("other-host", "a", "ffffffffffffffff", strconv.Itoa(deadPID)),
			sweepJobJSON("live", "a", p.hostID, strconv.Itoa(os.Getpid())),
			sweepJobJSON("dead", "b", p.hostID, strconv.Itoa(deadPID)),
			sweepJobJSON("mangled", "c", p.hostID, "not-a-pid"),
		}, ",") + `]}`
		fake.responses = []response{{stdout: list}, {}, {}}

		removed, err := p.SweepOrphans(context.Background())
		if err != nil {
			t.Fatalf("SweepOrphans: %v", err)
		}
		if !slices.Equal(removed, []string{"b/dead", "c/mangled"}) {
			t.Errorf("removed = %v, want the dead and mangled jobs of this host only", removed)
		}
		if !slices.Contains(fake.calls[0], "--all-namespaces") {
			t.Errorf("list call = %v, want --all-namespaces", fake.calls[0])
		}
	})

	t.Run("failures surface", func(t *testing.T) {
		p, _ := testProvider(t, response{exit: 1, stderr: "forbidden: cannot list jobs"})
		if _, err := p.SweepOrphans(context.Background()); err == nil || !strings.Contains(err.Error(), "forbidden") {
			t.Errorf("list failure: %v", err)
		}
		p, _ = testProvider(t, response{err: errors.New("spawn failed")})
		if _, err := p.SweepOrphans(context.Background()); err == nil {
			t.Error("runner error must surface")
		}
		p, _ = testProvider(t, response{stdout: "not json"})
		if _, err := p.SweepOrphans(context.Background()); err == nil || !strings.Contains(err.Error(), "parse") {
			t.Errorf("parse failure: %v", err)
		}
		p, _ = testProvider(t,
			response{stdout: `{"items":[` + sweepJobJSON("dead", "a", "abcd1234abcd1234", strconv.Itoa(deadPID)) + `]}`},
			response{exit: 1, stderr: "forbidden: cannot delete"})
		if _, err := p.SweepOrphans(context.Background()); err == nil || !strings.Contains(err.Error(), "sweep orphan a/dead") {
			t.Errorf("delete failure: %v", err)
		}
	})
}

// TestManifestAcceptsEveryDeclaredParam proves the published parameter
// list is one a drill config can actually use.
func TestManifestAcceptsEveryDeclaredParam(t *testing.T) {
	params := map[string]string{
		"image": "postgres:16", "namespace": "drills", "memory": "512Mi",
		"cpus": "1", "command": "sleep 1", "env.FOO": "bar",
	}
	for _, p := range Descriptor.Params {
		if _, ok := params[p.Name]; !ok && !p.Family {
			t.Fatalf("declared param %q has no sample value in this test", p.Name)
		}
	}
	m, namespace, err := New(nil).manifest(Descriptor, params)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	c := m.Spec.Template.Spec.Containers[0]
	switch {
	case namespace != "drills":
		t.Errorf("namespace %q, want drills", namespace)
	case c.Image != "postgres:16":
		t.Errorf("image %q", c.Image)
	case !slices.Equal(c.Command, []string{"sleep", "1"}):
		t.Errorf("command %v", c.Command)
	case c.Resources == nil || c.Resources.Limits["memory"] != "512Mi" || c.Resources.Limits["cpu"] != "1":
		t.Errorf("resources %+v", c.Resources)
	case len(c.Env) != 1 || c.Env[0].Name != "FOO" || c.Env[0].Value != "bar":
		t.Errorf("env %+v", c.Env)
	}
}

// TestManifestRejectsUnhandledDeclaredParam covers the defect path: a
// declared parameter the manifest builder never applies.
func TestManifestRejectsUnhandledDeclaredParam(t *testing.T) {
	d := Descriptor
	d.Params = append(append([]sandbox.Param{}, d.Params...),
		sandbox.Param{Name: "readonly", Doc: "Declared but not implemented."})
	_, _, err := New(nil).manifest(d, map[string]string{"image": "postgres:16", "readonly": "true"})
	if !errors.Is(err, sandbox.ErrInvalidParams) {
		t.Fatalf("error %v is not ErrInvalidParams", err)
	}
	if !strings.Contains(err.Error(), "declared but not implemented") {
		t.Errorf("error %q does not explain the defect", err)
	}
}

// TestSweepAsksTheOwnerLivenessCheck is the k8s half of the portability
// fix: see the docker provider's test of the same name. A stat of
// /proc/<pid> answered "gone" for every pid on a macOS drill host, so a
// concurrent drill's Job was deleted mid-restore.
func TestSweepAsksTheOwnerLivenessCheck(t *testing.T) {
	tests := []struct {
		name        string
		alive       bool
		wantRemoved []string
	}{
		{"live owner is spared", true, nil},
		{"dead owner is swept", false, []string{"a/job1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, fake := testProvider(t)
			list := `{"items":[` + sweepJobJSON("job1", "a", p.hostID, "4242") + `]}`
			fake.responses = []response{{stdout: list}, {}}
			asked := 0
			p.alive = func(id string) bool {
				asked++
				if id != "4242" {
					t.Errorf("liveness asked about %q, want the label's 4242", id)
				}
				return tt.alive
			}

			removed, err := p.SweepOrphans(context.Background())
			if err != nil {
				t.Fatalf("SweepOrphans: %v", err)
			}
			if !slices.Equal(removed, tt.wantRemoved) {
				t.Errorf("removed = %v, want %v", removed, tt.wantRemoved)
			}
			if asked != 1 {
				t.Errorf("liveness asked %d times, want exactly 1", asked)
			}
		})
	}
}

// TestExecKeepsSecretsOutOfArgv is the regression test for this provider's
// half of the leak: env used to be passed as `env NAME=value` in the exec
// command line, so a database password sat in the process list on the
// drill host and inside the pod. internal/checks refuses {{password}} in
// sql_runner argv for exactly that reason.
func TestExecKeepsSecretsOutOfArgv(t *testing.T) {
	const secret = "s3cr3t-ephemeral-password"
	sbx, fake := testSandbox(t, response{})

	if _, err := sbx.Exec(context.Background(), sandbox.ExecRequest{
		Argv: []string{"psql", "-c", "SELECT 1"},
		Env:  map[string]string{"PGPASSWORD": secret},
	}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	for _, arg := range fake.calls[0] {
		if strings.Contains(arg, secret) {
			t.Fatalf("argv %q carries the secret — visible in ps on the drill host and in the pod", arg)
		}
	}
	if !strings.HasPrefix(fake.stdins[0], "PGPASSWORD="+secret+"\n") {
		t.Errorf("stdin = %q, want the secret delivered out of band", fake.stdins[0])
	}
	if !slices.Contains(fake.calls[0], "-i") {
		t.Error("env without stdin still needs -i, or the prelude has nothing to read")
	}
}

// TestExecRejectsUnexpressibleEnv covers the one value a line protocol
// cannot carry. Truncating a credential silently — or exporting its tail
// as another variable — would be worse than refusing it.
func TestExecRejectsUnexpressibleEnv(t *testing.T) {
	sbx, fake := testSandbox(t)
	_, err := sbx.Exec(context.Background(), sandbox.ExecRequest{
		Argv: []string{"true"},
		Env:  map[string]string{"WEIRD": "two\nlines"},
	})
	if err == nil || !errors.Is(err, sandbox.ErrInvalidParams) {
		t.Errorf("err = %v, want an invalid-params rejection", err)
	}
	if len(fake.calls) != 0 {
		t.Error("nothing may be executed when the environment cannot be expressed")
	}
}

// TestQuantityParsing covers the shapes a pod spec actually carries.
// Kubernetes writes memory with binary or decimal suffixes and CPU either
// as a count or in millicores, and a limit nobody set is absent rather
// than zero — which is why every unreadable answer here is nil.
func TestQuantityParsing(t *testing.T) {
	memory := map[string]int64{
		"512Mi": 536870912, "2Gi": 2147483648, "1Ki": 1024,
		"1000000": 1000000, "500M": 500000000,
		"": 0, "banana": 0, "0": 0, "-1": 0,
	}
	for in, want := range memory {
		got := quantityBytes(in)
		if want == 0 && got != nil {
			t.Errorf("quantityBytes(%q) = %d, want nil", in, *got)
		} else if want != 0 && (got == nil || *got != want) {
			t.Errorf("quantityBytes(%q) = %v, want %d", in, got, want)
		}
	}
	cpu := map[string]int64{
		"1500m": 1500, "500m": 500, "2": 2000, "1.5": 1500,
		"": 0, "0": 0, "banana": 0,
	}
	for in, want := range cpu {
		got := quantityMilli(in)
		if want == 0 && got != nil {
			t.Errorf("quantityMilli(%q) = %d, want nil", in, *got)
		} else if want != 0 && (got == nil || *got != want) {
			t.Errorf("quantityMilli(%q) = %v, want %d", in, got, want)
		}
	}
}

// TestImageDigestOrNil: the kubelet writes imageID as "<repo>@sha256:<hex>"
// for an image it pulled by digest, and may write no digest at all for one
// loaded locally. Only the published form is recorded — a record naming an
// image must name bytes, not a tag.
func TestImageDigestOrNil(t *testing.T) {
	digest := "sha256:" + strings.Repeat("7a", 32)
	for in, want := range map[string]string{
		"docker.io/library/postgres@" + digest: digest,
		digest:                                 "",
		"postgres:16":                          "",
		"":                                     "",
		"repo@sha256:short":                    "",
	} {
		got := imageDigestOrNil(in)
		if want == "" && got != nil {
			t.Errorf("imageDigestOrNil(%q) = %q, want nil", in, *got)
		} else if want != "" && (got == nil || *got != want) {
			t.Errorf("imageDigestOrNil(%q) = %v, want %q", in, got, want)
		}
	}
}

// TestFactsReadsThePodRatherThanTheRequest: the values come from the API
// server's record of the pod, not from the parameters this provider was
// handed. A Job without limits is the ordinary case and reports nil for
// both — a pod with no limits takes what the node has, which is not a
// number a record may state.
func TestFactsReadsThePodRatherThanTheRequest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("7a", 32)
	tests := []struct {
		name       string
		stdout     string
		exit       int
		err        error
		wantImage  string
		wantMemory int64
		wantCPU    int64
	}{
		{"limits set", "docker.io/library/postgres@" + digest + " 2Gi 1500m", 0, nil, digest, 2147483648, 1500},
		{"no limits set", "docker.io/library/postgres@" + digest + " ", 0, nil, digest, 0, 0},
		{"nothing at all", "", 0, nil, "", 0, 0},
		{"kubectl failed", "", 1, nil, "", 0, 0},
		{"kubectl could not run", "", 0, errors.New("kubectl not found"), "", 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := testProvider(t, response{stdout: tc.stdout, exit: tc.exit, err: tc.err})
			got := (&Sandbox{job: "j", pod: "p", namespace: "n", p: p}).Facts(context.Background())
			checkStrPtr(t, "image_digest", got.ImageDigest, tc.wantImage)
			checkIntPtr(t, "memory_bytes", got.MemoryBytes, tc.wantMemory)
			checkIntPtr(t, "cpus_milli", got.CPUsMilli, tc.wantCPU)
		})
	}
}

func checkStrPtr(t *testing.T, name string, got *string, want string) {
	t.Helper()
	switch {
	case want == "" && got != nil:
		t.Errorf("%s = %q, want nil", name, *got)
	case want != "" && (got == nil || *got != want):
		t.Errorf("%s = %v, want %q", name, got, want)
	}
}

func checkIntPtr(t *testing.T, name string, got *int64, want int64) {
	t.Helper()
	switch {
	case want == 0 && got != nil:
		t.Errorf("%s = %d, want nil — unlimited and unknown are both absences", name, *got)
	case want != 0 && (got == nil || *got != want):
		t.Errorf("%s = %v, want %d", name, got, want)
	}
}
