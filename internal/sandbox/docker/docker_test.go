package docker

import (
	"context"
	"errors"
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

const testHostID = "abcd1234abcd1234"

func testProvider(t *testing.T, responses ...response) (*Provider, *fakeRunner) {
	t.Helper()
	fake := &fakeRunner{t: t, responses: responses}
	return &Provider{
		bin:           "docker",
		run:           fake,
		logger:        slog.New(slog.DiscardHandler),
		pid:           os.Getpid(),
		hostID:        testHostID,
		awaitInterval: time.Millisecond,
		awaitCap:      50 * time.Millisecond,
		alive:         sandbox.OwnerAlive,
	}, fake
}

func TestNewDefaults(t *testing.T) {
	p := New(nil)
	if p.bin != "docker" || p.run == nil || p.logger == nil {
		t.Errorf("New: %+v, want docker binary with runner and logger", p)
	}
	if p.awaitInterval != awaitInterval || p.awaitCap != maxAwaitUptime || p.pid != os.Getpid() {
		t.Errorf("New: interval=%v cap=%v pid=%d, want package defaults", p.awaitInterval, p.awaitCap, p.pid)
	}
	if p.alive == nil {
		t.Error("New: alive is nil — the orphan sweep would panic on its first labeled container")
	}
	if len(p.hostID) != 16 {
		t.Errorf("hostID = %q, want 16 hex chars", p.hostID)
	}
	if custom := New(slog.New(slog.DiscardHandler)); custom.logger == nil {
		t.Error("New must keep a caller-provided logger")
	}
}

func TestRunArgsDefaults(t *testing.T) {
	p, _ := testProvider(t)
	args, err := p.runArgs(Descriptor, map[string]string{"image": "postgres:16"})
	if err != nil {
		t.Fatalf("runArgs: %v", err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--network none",
		"--label " + LabelSandbox + "=1",
		"--label " + labelPID + "=" + strconv.Itoa(os.Getpid()),
		"--label " + labelHost + "=" + testHostID,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q missing %q", joined, want)
		}
	}
	if strings.Contains(joined, "-p ") || strings.Contains(joined, "--publish") {
		t.Errorf("args %q must never publish ports", joined)
	}
	if args[len(args)-1] != "postgres:16" {
		t.Errorf("image must be the last argument, got %q", args[len(args)-1])
	}
}

func TestRunArgsMapping(t *testing.T) {
	p, _ := testProvider(t)
	args, err := p.runArgs(Descriptor, map[string]string{
		"image": "postgres:16", "memory": "2GiB", "cpus": "2",
		"network": "bridge", "env.POSTGRES_PASSWORD": "secret",
	})
	if err != nil {
		t.Fatalf("runArgs: %v", err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"--memory 2GiB", "--cpus 2", "--network bridge", "-e POSTGRES_PASSWORD=secret"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q missing %q", joined, want)
		}
	}
}

func TestRunArgsCommandOverride(t *testing.T) {
	p, _ := testProvider(t)
	args, err := p.runArgs(Descriptor, map[string]string{"image": "x:1", "command": "sleep  infinity"})
	if err != nil {
		t.Fatalf("runArgs: %v", err)
	}
	if got := strings.Join(args[len(args)-3:], " "); got != "x:1 sleep infinity" {
		t.Errorf("tail = %q, want the command argv split after the image", got)
	}
	// Without command the image stays last.
	args, err = p.runArgs(Descriptor, map[string]string{"image": "x:1"})
	if err != nil || args[len(args)-1] != "x:1" {
		t.Errorf("args tail = %v err=%v", args[len(args)-1], err)
	}
}

func TestRunArgsRejects(t *testing.T) {
	p, _ := testProvider(t)
	for name, params := range map[string]map[string]string{
		"missing image":  {"memory": "2GiB"},
		"unknown param":  {"image": "x", "ports": "5432:5432"},
		"bad env name":   {"image": "x", "env.1BAD": "v"},
		"empty env name": {"image": "x", "env.": "v"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := p.runArgs(Descriptor, params); !errors.Is(err, sandbox.ErrInvalidParams) {
				t.Errorf("runArgs(%v): got %v, want ErrInvalidParams", params, err)
			}
		})
	}
}

func TestCreate(t *testing.T) {
	t.Run("waits for running state", func(t *testing.T) {
		p, fake := testProvider(t,
			response{stdout: "abc123\n"}, // docker run
			response{stdout: "false\n"},  // inspect: not yet
			response{stdout: "true\n"},   // inspect: running
		)
		sbx, err := p.Create(context.Background(), map[string]string{"image": "alpine:3"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if sbx.ID() != "abc123" {
			t.Errorf("ID = %q, want abc123", sbx.ID())
		}
		if len(fake.calls) != 3 {
			t.Errorf("calls = %d, want 3", len(fake.calls))
		}
	})

	t.Run("docker run failure", func(t *testing.T) {
		p, _ := testProvider(t, response{exit: 125, stderr: "no such image\nmore"})
		if _, err := p.Create(context.Background(), map[string]string{"image": "x"}); err == nil || !strings.Contains(err.Error(), "no such image") {
			t.Errorf("Create: got %v, want docker run failure with first stderr line", err)
		}
	})

	t.Run("never-running container is destroyed", func(t *testing.T) {
		inspects := make([]response, 0, 64)
		for range 60 {
			inspects = append(inspects, response{stdout: "false\n"})
		}
		p, fake := testProvider(t, append([]response{{stdout: "abc123\n"}},
			append(inspects, response{exit: 0} /* docker rm */)...)...)
		_, err := p.Create(context.Background(), map[string]string{"image": "x"})
		if err == nil || !strings.Contains(err.Error(), "never reached running state") {
			t.Fatalf("Create: got %v, want await failure", err)
		}
		last := fake.calls[len(fake.calls)-1]
		if !slices.Contains(last, "rm") || !slices.Contains(last, "abc123") {
			t.Errorf("failure path must destroy the container; last call: %v", last)
		}
	})
}

func TestExec(t *testing.T) {
	t.Run("full request mapping", func(t *testing.T) {
		p, fake := testProvider(t, response{stdout: "out", stderr: "err", truncated: true, exit: 7})
		sbx := &Sandbox{id: "abc123", p: p}
		res, err := sbx.Exec(context.Background(), sandbox.ExecRequest{
			Argv:    []string{"psql", "-c", "SELECT 1"},
			Env:     map[string]string{"B": "2", "A": "1"},
			Stdin:   []byte("stdin-data"),
			Timeout: time.Second,
		})
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if res.ExitCode != 7 || string(res.Stdout) != "out" || string(res.Stderr) != "err" || !res.Truncated {
			t.Errorf("result = %+v, want exit 7 with captured streams and truncation", res)
		}
		if res.Duration <= 0 {
			t.Error("duration must be measured")
		}
		assertExecCall(t, fake)
	})

	t.Run("no -i without stdin", func(t *testing.T) {
		p, fake := testProvider(t, response{})
		sbx := &Sandbox{id: "abc123", p: p}
		if _, err := sbx.Exec(context.Background(), sandbox.ExecRequest{Argv: []string{"true"}}); err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if slices.Contains(fake.calls[0], "-i") {
			t.Errorf("call %v must not contain -i without stdin", fake.calls[0])
		}
	})

	t.Run("runner error surfaces", func(t *testing.T) {
		p, _ := testProvider(t, response{err: errors.New("context deadline exceeded")})
		sbx := &Sandbox{id: "abc123", p: p}
		if _, err := sbx.Exec(context.Background(), sandbox.ExecRequest{Argv: []string{"sleep"}}); err == nil {
			t.Error("Exec must surface runner errors")
		}
	})
}

func TestPutFile(t *testing.T) {
	hostFile := filepath.Join(t.TempDir(), "backup.dump")
	if err := os.WriteFile(hostFile, []byte("0123456789"), 0o600); err != nil {
		t.Fatalf("write host file: %v", err)
	}

	t.Run("copy, chown to the exec identity, chmod", func(t *testing.T) {
		p, fake := testProvider(t, response{}, response{stdout: "10001:10001\n"}, response{})
		sbx := &Sandbox{id: "abc123", p: p}
		res, err := sbx.PutFile(context.Background(), hostFile, "/tmp/b.dump", "")
		if err != nil {
			t.Fatalf("PutFile: %v", err)
		}
		if res.BytesCopied != 10 {
			t.Errorf("BytesCopied = %d, want 10", res.BytesCopied)
		}
		if got := strings.Join(fake.calls[0], " "); got != "docker cp "+hostFile+" abc123:/tmp/b.dump" {
			t.Errorf("cp call = %q", got)
		}
		if got := strings.Join(fake.calls[1], " "); got != `docker exec abc123 sh -c echo "$(id -u):$(id -g)"` {
			t.Errorf("identity call = %q", got)
		}
		want := `docker exec -u 0 abc123 sh -c chown -R -- "$1" "$2" && chmod -- "$3" "$2" sh 10001:10001 /tmp/b.dump 0600`
		if got := strings.Join(fake.calls[2], " "); got != want {
			t.Errorf("ownership call = %q\nwant           %q (root-run, default mode 0600)", got, want)
		}
	})

}

func TestPutFileFailures(t *testing.T) {
	hostFile := filepath.Join(t.TempDir(), "backup.dump")
	if err := os.WriteFile(hostFile, []byte("0123456789"), 0o600); err != nil {
		t.Fatalf("write host file: %v", err)
	}

	t.Run("invalid inputs fail before any docker call", func(t *testing.T) {
		p, _ := testProvider(t)
		sbx := &Sandbox{id: "abc123", p: p}
		if _, err := sbx.PutFile(context.Background(), hostFile, "/tmp/b", "rw-"); !errors.Is(err, sandbox.ErrInvalidParams) {
			t.Errorf("non-octal mode: got %v, want ErrInvalidParams", err)
		}
		if _, err := sbx.PutFile(context.Background(), filepath.Join(t.TempDir(), "gone"), "/tmp/b", ""); err == nil {
			t.Error("missing host file must fail before any docker call")
		}
	})

	for name, tt := range map[string]struct {
		responses []response
		wantSub   string
	}{
		"cp fails":       {[]response{{exit: 1, stderr: "cp failed"}}, "cp failed"},
		"identity fails": {[]response{{}, {exit: 1, stderr: "sh: not found"}}, "exec identity"},
		// A root-run chown must never receive anything but uid:gid — junk
		// id output is refused, not passed through.
		"junk identity": {[]response{{}, {stdout: "uid=0(root) gid=0\n"}}, "unexpected id output"},
		"chown fails":   {[]response{{}, {stdout: "0:0\n"}, {exit: 1, stderr: "chown: not permitted"}}, "ownership and mode"},
	} {
		t.Run(name, func(t *testing.T) {
			p, _ := testProvider(t, tt.responses...)
			sbx := &Sandbox{id: "abc123", p: p}
			if _, err := sbx.PutFile(context.Background(), hostFile, "/tmp/b", ""); err == nil || !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("PutFile: got %v, want mention of %q", err, tt.wantSub)
			}
		})
	}
}

func TestDestroyIdempotent(t *testing.T) {
	p, _ := testProvider(t,
		response{exit: 0},
		response{exit: 1, stderr: "Error response from daemon: No such container: abc123"},
		// The same refusal in podman's wording. Its own rm -f exits 0, so
		// this arrives only from a runtime that words it lower case.
		response{exit: 1, stderr: `Error: no such container "abc123"`},
		response{exit: 1, stderr: "permission denied"},
	)
	sbx := &Sandbox{id: "abc123", p: p}
	if err := sbx.Destroy(context.Background()); err != nil {
		t.Errorf("first Destroy: %v", err)
	}
	if err := sbx.Destroy(context.Background()); err != nil {
		t.Errorf("Destroy of removed container must succeed (idempotent), got: %v", err)
	}
	if err := sbx.Destroy(context.Background()); err != nil {
		t.Errorf("Destroy must read a lower-cased refusal the same way, got: %v", err)
	}
	if err := sbx.Destroy(context.Background()); err == nil {
		t.Error("genuine removal failure must surface")
	}
}

func TestSweepOrphans(t *testing.T) {
	livePID := strconv.Itoa(os.Getpid())
	p, fake := testProvider(t,
		response{stdout: "dead1\nlive2\nbroken3\ngone4\nforeign5\nlegacy6\ngone7\n"}, // docker ps
		response{stdout: testHostID + "|999999999\n"},                                // inspect dead1: mine, pid long gone
		response{exit: 0}, // rm dead1
		response{stdout: testHostID + "|" + livePID + "\n"}, // inspect live2: mine, this process
		response{stdout: "|\n"},                             // inspect broken3: labels lost
		response{exit: 0},                                   // rm broken3
		// gone4 was torn down by a concurrent drill between ps and inspect
		// — the sweep must skip it, not fail.
		response{exit: 1, stderr: "Error: No such object: gone4"},
		// foreign5 belongs to another drill host sharing this daemon
		// (DOCKER_HOST=ssh://…); its pid means nothing here — never sweep,
		// even though that pid is dead locally.
		response{stdout: "ffff0000ffff0000|999999999\n"},
		// legacy6 predates the host label: host-local by definition, so
		// the dead pid makes it an orphan.
		response{stdout: "|999999998\n"},
		response{exit: 0}, // rm legacy6
		// gone7 vanished the same way gone4 did, refused in podman's
		// wording (measured: `Error: no such object: "…"`, exit 125). The
		// sweep runs at every drill start, so reading this as an error
		// would fail the drill rather than skip a container that is
		// already gone.
		response{exit: 125, stderr: `Error: no such object: "gone7"`},
	)
	removed, err := p.SweepOrphans(context.Background())
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if !slices.Equal(removed, []string{"dead1", "broken3", "legacy6"}) {
		t.Errorf("removed = %v, want [dead1 broken3 legacy6] — live-owner and foreign-host sandboxes must survive", removed)
	}
	for _, call := range fake.calls {
		if slices.Contains(call, "rm") && (slices.Contains(call, "live2") || slices.Contains(call, "foreign5")) {
			t.Errorf("sweep removed a live or foreign sandbox: %v", call)
		}
	}

	p, _ = testProvider(t, response{exit: 1, stderr: "daemon down"})
	if _, err := p.SweepOrphans(context.Background()); err == nil {
		t.Error("docker ps failure must surface")
	}
}

func TestSweepOrphansErrorPaths(t *testing.T) {
	t.Run("inspect failure", func(t *testing.T) {
		p, _ := testProvider(t,
			response{stdout: "id1\n"},
			response{err: errors.New("daemon gone")},
		)
		if _, err := p.SweepOrphans(context.Background()); err == nil {
			t.Error("inspect failure must surface")
		}
	})
	t.Run("inspect nonzero exit", func(t *testing.T) {
		p, _ := testProvider(t,
			response{stdout: "id1\n"},
			// Anything but the vanished-container refusal: that one is
			// read as "already gone" in either runtime's casing, so a
			// fixture spelling it would assert the opposite of this case.
			response{exit: 1, stderr: "permission denied while trying to connect to the daemon socket"},
		)
		if _, err := p.SweepOrphans(context.Background()); err == nil {
			t.Error("inspect exit failure must surface")
		}
	})
	t.Run("removal failure", func(t *testing.T) {
		p, _ := testProvider(t,
			response{stdout: "id1\n"},
			response{stdout: testHostID + "|999999999\n"},
			response{exit: 1, stderr: "permission denied"},
		)
		if _, err := p.SweepOrphans(context.Background()); err == nil || !strings.Contains(err.Error(), "sweep orphan") {
			t.Errorf("removal failure: got %v", err)
		}
	})
	t.Run("list runner error", func(t *testing.T) {
		p, _ := testProvider(t, response{err: errors.New("no docker binary")})
		if _, err := p.SweepOrphans(context.Background()); err == nil {
			t.Error("list runner error must surface")
		}
	})
}

func TestRunnerErrorPaths(t *testing.T) {
	hostFile := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(hostFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("write host file: %v", err)
	}

	t.Run("create runner error", func(t *testing.T) {
		p, _ := testProvider(t, response{err: errors.New("spawn failed")})
		if _, err := p.Create(context.Background(), map[string]string{"image": "x"}); err == nil {
			t.Error("create runner error must surface")
		}
	})
	t.Run("await runner error destroys container", func(t *testing.T) {
		p, fake := testProvider(t,
			response{stdout: "abc123\n"},
			response{err: errors.New("inspect died")},
			response{exit: 0}, // docker rm on the failure path
		)
		if _, err := p.Create(context.Background(), map[string]string{"image": "x"}); err == nil {
			t.Fatal("await runner error must surface")
		}
		last := fake.calls[len(fake.calls)-1]
		if !slices.Contains(last, "rm") {
			t.Errorf("failure path must destroy the container; last call: %v", last)
		}
	})
	t.Run("putfile cp runner error", func(t *testing.T) {
		p, _ := testProvider(t, response{err: errors.New("cp spawn failed")})
		sbx := &Sandbox{id: "abc123", p: p}
		if _, err := sbx.PutFile(context.Background(), hostFile, "/tmp/f", ""); err == nil {
			t.Error("cp runner error must surface")
		}
	})
	t.Run("putfile identity runner error", func(t *testing.T) {
		p, _ := testProvider(t, response{}, response{err: errors.New("id spawn failed")})
		sbx := &Sandbox{id: "abc123", p: p}
		if _, err := sbx.PutFile(context.Background(), hostFile, "/tmp/f", ""); err == nil {
			t.Error("identity runner error must surface")
		}
	})
	t.Run("putfile chown runner error", func(t *testing.T) {
		p, _ := testProvider(t, response{}, response{stdout: "0:0\n"}, response{err: errors.New("chown spawn failed")})
		sbx := &Sandbox{id: "abc123", p: p}
		if _, err := sbx.PutFile(context.Background(), hostFile, "/tmp/f", ""); err == nil {
			t.Error("chown runner error must surface")
		}
	})
	t.Run("destroy runner error", func(t *testing.T) {
		p, _ := testProvider(t, response{err: errors.New("rm spawn failed")})
		sbx := &Sandbox{id: "abc123", p: p}
		if err := sbx.Destroy(context.Background()); err == nil {
			t.Error("destroy runner error must surface")
		}
	})
}

// descriptor_test additions: the descriptor is the parameter gate, and it
// is what docs/capabilities.json publishes. These two tests pin it in both
// directions — everything declared works, and nothing works undeclared.

// TestRunArgsAcceptsEveryDeclaredParam proves the published parameter list
// is one a drill config can actually use.
func TestRunArgsAcceptsEveryDeclaredParam(t *testing.T) {
	params := map[string]string{
		"image": "postgres:16", "network": "none", "memory": "512m",
		"cpus": "1", "command": "sleep 1", "env.FOO": "bar",
	}
	for _, p := range Descriptor.Params {
		if _, ok := params[p.Name]; !ok && !p.Family {
			t.Fatalf("declared param %q has no sample value in this test", p.Name)
		}
	}
	args, err := New(nil).runArgs(Descriptor, params)
	if err != nil {
		t.Fatalf("runArgs: %v", err)
	}
	for _, want := range [][]string{
		{"--network", "none"}, {"--memory", "512m"}, {"--cpus", "1"},
		{"-e", "FOO=bar"}, {"postgres:16"}, {"sleep", "1"},
	} {
		if !containsSequence(args, want) {
			t.Errorf("argv %v does not apply %v", args, want)
		}
	}
}

// TestRunArgsRejectsUnhandledDeclaredParam covers the defect path: a
// parameter the descriptor declares but runArgs never applies. Dropping it
// silently would build a sandbox that is not the one the drill asked for.
func TestRunArgsRejectsUnhandledDeclaredParam(t *testing.T) {
	d := Descriptor
	d.Params = append(append([]sandbox.Param{}, d.Params...),
		sandbox.Param{Name: "readonly", Doc: "Declared but not implemented."})
	_, err := New(nil).runArgs(d, map[string]string{"image": "postgres:16", "readonly": "true"})
	if !errors.Is(err, sandbox.ErrInvalidParams) {
		t.Fatalf("error %v is not ErrInvalidParams", err)
	}
	if !strings.Contains(err.Error(), "declared but not implemented") {
		t.Errorf("error %q does not explain the defect", err)
	}
}

func containsSequence(haystack, needle []string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if slices.Equal(haystack[i:i+len(needle)], needle) {
			return true
		}
	}
	return false
}

// TestExecKeepsSecretsOutOfArgv is the regression test for the leak this
// provider used to have: the resolved database password reached
// `docker exec -e NAME=value`, so it sat in the drill host's process list
// for every local user to read. internal/checks refuses {{password}} in
// sql_runner argv for exactly that reason — the provider was undoing the
// protection one layer down.
func TestExecKeepsSecretsOutOfArgv(t *testing.T) {
	const secret = "s3cr3t-ephemeral-password"
	p, fake := testProvider(t, response{})
	sbx := &Sandbox{id: "abc123", p: p}

	if _, err := sbx.Exec(context.Background(), sandbox.ExecRequest{
		Argv: []string{"psql", "-c", "SELECT 1"},
		Env:  map[string]string{"PGPASSWORD": secret},
	}); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	for _, arg := range fake.calls[0] {
		if strings.Contains(arg, secret) {
			t.Fatalf("argv %q carries the secret — visible in ps to every local user", arg)
		}
	}
	if !slices.Contains(fake.calls[0], "PGPASSWORD") {
		t.Error("argv must still name the variable so docker forwards it")
	}
	if !slices.Equal(fake.envs[0], []string{"PGPASSWORD=" + secret}) {
		t.Errorf("child env = %v, want the secret delivered out of band", fake.envs[0])
	}
}

// assertExecCall checks the shape of a full exec invocation: env by name in
// argv, values out of band in the child's environment, -i only with stdin.
func assertExecCall(t *testing.T, fake *fakeRunner) {
	t.Helper()
	call := strings.Join(fake.calls[0], " ")
	want := "docker exec -i -e A -e B abc123 psql -c SELECT 1"
	if call != want {
		t.Errorf("call = %q, want %q (env by name only, sorted; -i only with stdin)", call, want)
	}
	// The values travel in the docker CLI's own environment. Naming them in
	// argv would put every one of them in `ps` output for any local user to
	// read, which is exactly what the sql_runner template refuses to do with
	// {{password}}.
	if !slices.Equal(fake.envs[0], []string{"A=1", "B=2"}) {
		t.Errorf("child env = %v, want the values passed out of band", fake.envs[0])
	}
	for _, arg := range fake.calls[0] {
		if strings.Contains(arg, "=1") || strings.Contains(arg, "=2") {
			t.Errorf("argv %q carries an environment value", arg)
		}
	}
	if fake.stdins[0] != "stdin-data" {
		t.Errorf("stdin = %q, want stdin-data", fake.stdins[0])
	}
}

// TestSweepAsksTheOwnerLivenessCheck pins the decision the sweep used to
// get wrong off Linux. The old implementation stat'ed /proc/<pid>, which
// does not exist on macOS — where Probavi also ships binaries — so every
// labeled container looked orphaned and a starting drill destroyed the
// running sandbox of a concurrent one. The verdict now comes from a
// liveness check with no platform hole, and this test drives both answers.
func TestSweepAsksTheOwnerLivenessCheck(t *testing.T) {
	tests := []struct {
		name        string
		alive       bool
		wantRemoved []string
	}{
		{"live owner is spared", true, []string{}},
		{"dead owner is swept", false, []string{"sbx1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			responses := []response{
				{stdout: "sbx1\n"},
				{stdout: testHostID + "|4242\n"},
			}
			if !tt.alive {
				responses = append(responses, response{exit: 0}) // rm sbx1
			}
			p, _ := testProvider(t, responses...)
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
