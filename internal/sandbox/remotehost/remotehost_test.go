package remotehost

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

const (
	testHostID = "abcd1234abcd1234"
	testTarget = "drill@target.example"
	testRoot   = "/var/lib/probavi-drills"
)

func testProvider(t *testing.T, responses ...response) (*Provider, *fakeRunner) {
	t.Helper()
	fake := &fakeRunner{t: t, responses: responses}
	return &Provider{
		bin:           "ssh",
		run:           fake,
		logger:        slog.New(slog.DiscardHandler),
		pid:           os.Getpid(),
		hostID:        testHostID,
		target:        testTarget,
		workspaceRoot: testRoot,
		alive:         sandbox.OwnerAlive,
	}, fake
}

// remoteCmd asserts the transport shape of call i and returns the remote
// command string ssh would hand to the target's login shell.
func remoteCmd(t *testing.T, fake *fakeRunner, i int) string {
	t.Helper()
	call := fake.calls[i]
	if len(call) != 5 || call[0] != "ssh" || call[1] != "-o" || call[2] != "BatchMode=yes" || call[3] != testTarget {
		t.Fatalf("call %d = %v, want ssh -o BatchMode=yes %s <command>", i, call, testTarget)
	}
	return call[4]
}

// quoteJoin builds the expected remote command from argv the way the
// provider does; shQuote itself is pinned separately in TestShQuote.
func quoteJoin(argv ...string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = shQuote(a)
	}
	return strings.Join(quoted, " ")
}

func TestShQuote(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"plain", "'plain'"},
		{"", "''"},
		{"two words", "'two words'"},
		{"SELECT 'x'", `'SELECT '\''x'\'''`},
		{"$HOME `id` \" ; rm", "'$HOME `id` \" ; rm'"},
	} {
		if got := shQuote(tt.in); got != tt.want {
			t.Errorf("shQuote(%q) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

func TestNewDefaults(t *testing.T) {
	t.Setenv(EnvTarget, testTarget)
	p, err := New(nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.bin != "ssh" || p.run == nil || p.logger == nil || p.pid != os.Getpid() {
		t.Errorf("New: %+v, want ssh binary with runner, logger, and pid", p)
	}
	if p.alive == nil {
		t.Error("New: alive is nil — the orphan sweep would panic on its first marker")
	}
	if len(p.hostID) != 16 {
		t.Errorf("hostID = %q, want 16 hex chars", p.hostID)
	}
	if p.target != testTarget || p.workspaceRoot != defaultWorkspaceRoot {
		t.Errorf("target = %q root = %q, want %q and the default root", p.target, p.workspaceRoot, testTarget)
	}
	if custom, err := New(slog.New(slog.DiscardHandler), nil); err != nil || custom.logger == nil {
		t.Error("New must keep a caller-provided logger")
	}
	if p, err := New(nil, map[string]string{"workspace_root": "/srv/drills/"}); err != nil || p.workspaceRoot != "/srv/drills" {
		t.Errorf("root = %q err = %v, want the cleaned configured root", p.workspaceRoot, err)
	}
}

func TestNewRejects(t *testing.T) {
	for name, tt := range map[string]struct {
		target  string
		params  map[string]string
		wantSub string
	}{
		"missing target":       {"", nil, EnvTarget},
		"option-shaped target": {"-oProxyCommand=evil", nil, "ssh option"},
		"bad params fail fast": {testTarget, map[string]string{"image": "x"}, "invalid sandbox params"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(EnvTarget, tt.target)
			if _, err := New(nil, tt.params); err == nil || !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("New: %v, want mention of %q", err, tt.wantSub)
			}
		})
	}
}

func TestParseParams(t *testing.T) {
	t.Run("mapping", func(t *testing.T) {
		set, err := parseParams(Descriptor, map[string]string{"memory": "2G", "cpus": "0.5", "workspace_root": "/srv/d"})
		if err != nil {
			t.Fatalf("parseParams: %v", err)
		}
		if set.root != "/srv/d" {
			t.Errorf("root = %q, want /srv/d", set.root)
		}
		// Sorted param order: cpus before memory.
		if !slices.Equal(set.props, []string{"CPUQuota=50%", "MemoryMax=2G"}) {
			t.Errorf("props = %v, want [CPUQuota=50%% MemoryMax=2G]", set.props)
		}
	})

	t.Run("defaults", func(t *testing.T) {
		set, err := parseParams(Descriptor, nil)
		if err != nil || set.root != defaultWorkspaceRoot || len(set.props) != 0 {
			t.Errorf("parseParams(Descriptor, nil) = %+v, %v — want the default root and no caps", set, err)
		}
	})

	t.Run("rejects", func(t *testing.T) {
		for name, params := range map[string]map[string]string{
			"unknown param":   {"image": "postgres:16"},
			"relative root":   {"workspace_root": "drills"},
			"empty root":      {"workspace_root": ""},
			"docker memory":   {"memory": "2GiB"},
			"negative memory": {"memory": "-1G"},
			"zero cpus":       {"cpus": "0"},
			"negative cpus":   {"cpus": "-2"},
			"word cpus":       {"cpus": "two"},
		} {
			t.Run(name, func(t *testing.T) {
				if _, err := parseParams(Descriptor, params); !errors.Is(err, sandbox.ErrInvalidParams) {
					t.Errorf("parseParams(Descriptor, %v): got %v, want ErrInvalidParams", params, err)
				}
			})
		}
	})
}

// versionOK and userOK script the two probe calls every successful Create
// starts with.
var (
	versionOK = response{stdout: "systemd 255 (255.4-1ubuntu8)\n+PAM +AUDIT\n"}
	userOK    = response{stdout: "drill\n"}
)

func TestCreateFullSequence(t *testing.T) {
	p, fake := testProvider(t,
		versionOK,
		userOK,
		response{}, // workspace + marker
		response{}, // verify systemd-run
		response{}, // set-property
		response{}, // reaper timer
	)
	sbx, err := p.Create(context.Background(), map[string]string{"memory": "2G", "cpus": "2"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	name := sbx.ID()
	if !strings.HasPrefix(name, namePrefix) {
		t.Fatalf("ID = %q, want the %s prefix", name, namePrefix)
	}
	ws := testRoot + "/" + name
	if sbx.workspace != ws || sbx.user != "drill" {
		t.Errorf("sandbox = %+v, want workspace %s as user drill", sbx, ws)
	}
	if got := sbx.ScratchDir(); got != ws+"/scratch" {
		t.Errorf("ScratchDir = %q, want %s/scratch", got, ws)
	}
	want := []string{
		quoteJoin("systemctl", "--version"),
		quoteJoin("id", "-un"),
		quoteJoin("sh", "-c", setupScript, "sh", ws, testHostID+" "+sandbox.OwnerID(os.Getpid())),
		quoteJoin("systemd-run", "--quiet", "--collect", "--wait", "--pipe",
			"--slice="+name+".slice", "-p", "User=drill", "-p", "WorkingDirectory="+ws, "--", "true"),
		quoteJoin("systemctl", "set-property", "--runtime", name+".slice", "CPUQuota=200%", "MemoryMax=2G"),
		quoteJoin("systemd-run", "--quiet", "--collect", "--unit="+name+"-reaper",
			"--on-active=7200", "--timer-property=AccuracySec=1m",
			"sh", "-c", "systemctl stop "+shQuote(name+".slice")+"; rm -rf "+shQuote(ws)),
	}
	if len(fake.calls) != len(want) {
		t.Fatalf("calls = %d, want %d", len(fake.calls), len(want))
	}
	for i, w := range want {
		if got := remoteCmd(t, fake, i); got != w {
			t.Errorf("call %d = %s\nwant       %s", i, got, w)
		}
	}
}

func TestCreateWithoutCaps(t *testing.T) {
	p, fake := testProvider(t, versionOK, userOK, response{}, response{}, response{})
	if _, err := p.Create(context.Background(), nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, call := range fake.calls {
		if strings.Contains(strings.Join(call, " "), "set-property") {
			t.Errorf("no caps were configured, yet set-property ran: %v", call)
		}
	}
}

func TestCreateBadParams(t *testing.T) {
	p, fake := testProvider(t)
	if _, err := p.Create(context.Background(), map[string]string{"image": "x"}); !errors.Is(err, sandbox.ErrInvalidParams) {
		t.Errorf("Create: %v, want ErrInvalidParams", err)
	}
	if len(fake.calls) != 0 {
		t.Errorf("bad params must not reach the target; calls: %v", fake.calls)
	}
}

// TestCreateProbeFailures covers everything that fails before the
// workspace exists: no partial sandbox, so no cleanup call either.
func TestCreateProbeFailures(t *testing.T) {
	for name, tt := range map[string]struct {
		responses []response
		wantSub   string
	}{
		"systemd too old":     {[]response{{stdout: "systemd 243 (v243)\n"}}, "244"},
		"empty version":       {[]response{{stdout: ""}}, "parse"},
		"not systemd":         {[]response{{stdout: "upstart 1.13\n"}}, "parse"},
		"non-numeric version": {[]response{{stdout: "systemd two\n"}}, "parse"},
		"probe exit failure":  {[]response{{exit: 255, stderr: "ssh: connect refused"}}, "connect refused"},
		"probe runner error":  {[]response{{err: errors.New("spawn failed")}}, "spawn failed"},
		"id exit failure":     {[]response{versionOK, {exit: 1, stderr: "id: broken"}}, "id exited"},
		"empty id output":     {[]response{versionOK, {stdout: "\n"}}, "nothing"},
		"id runner error":     {[]response{versionOK, {err: errors.New("spawn failed")}}, "spawn failed"},
	} {
		t.Run(name, func(t *testing.T) {
			p, fake := testProvider(t, tt.responses...)
			if _, err := p.Create(context.Background(), nil); err == nil || !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("Create: %v, want mention of %q", err, tt.wantSub)
			}
			if len(fake.calls) != len(tt.responses) {
				t.Errorf("calls = %d, want %d — probe failures must not run setup or cleanup", len(fake.calls), len(tt.responses))
			}
		})
	}
}

// TestCreateSetupFailures: each failure after the workspace exists must
// destroy the partial sandbox — the destroy script is always the last
// call. Both remote exits and runner errors are covered per step.
func TestCreateSetupFailures(t *testing.T) {
	spawn := errors.New("spawn failed")
	for name, tt := range map[string]struct {
		responses []response
		wantErr   string
	}{
		"workspace creation fails": {
			[]response{versionOK, userOK, {exit: 1, stderr: "mkdir: permission denied"}, {}},
			"permission denied",
		},
		"verify fails (polkit)": {
			[]response{versionOK, userOK, {}, {exit: 4, stderr: "Interactive authentication required."}, {}},
			"polkit",
		},
		"set-property fails": {
			[]response{versionOK, userOK, {}, {}, {exit: 1, stderr: "Failed to set MemoryMax"}, {}},
			"resource caps",
		},
		"reaper fails": {
			[]response{versionOK, userOK, {}, {}, {}, {exit: 1, stderr: "timer refused"}, {}},
			"deadline backstop",
		},
		"workspace runner error":    {[]response{versionOK, userOK, {err: spawn}, {}}, "spawn failed"},
		"verify runner error":       {[]response{versionOK, userOK, {}, {err: spawn}, {}}, "spawn failed"},
		"set-property runner error": {[]response{versionOK, userOK, {}, {}, {err: spawn}, {}}, "spawn failed"},
		"reaper runner error":       {[]response{versionOK, userOK, {}, {}, {}, {err: spawn}, {}}, "spawn failed"},
	} {
		t.Run(name, func(t *testing.T) {
			p, fake := testProvider(t, tt.responses...)
			_, err := p.Create(context.Background(), map[string]string{"memory": "1G"})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Create: %v, want mention of %q", err, tt.wantErr)
			}
			last := remoteCmd(t, fake, len(fake.calls)-1)
			if !strings.Contains(last, shQuote(destroyScript)) {
				t.Errorf("failure path must destroy the partial sandbox; last call: %s", last)
			}
		})
	}
}

func TestCreateCleanupFailureIsLoggedNotFatal(t *testing.T) {
	p, _ := testProvider(t,
		versionOK, userOK, response{},
		response{exit: 4, stderr: "denied"},
		response{err: errors.New("target gone")}, // destroy fails too
	)
	if _, err := p.Create(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Errorf("Create must report the original failure, got %v", err)
	}
}

func testSandbox(p *Provider) *Sandbox {
	name := namePrefix + "cafe0123"
	return &Sandbox{name: name, workspace: testRoot + "/" + name, user: "drill", p: p}
}

func TestExec(t *testing.T) {
	t.Run("full request mapping", func(t *testing.T) {
		p, fake := testProvider(t, response{stdout: "out", stderr: "err", truncated: true, exit: 7})
		sbx := testSandbox(p)
		res, err := sbx.Exec(context.Background(), sandbox.ExecRequest{
			Argv:    []string{"psql", "-c", "SELECT 'x'"},
			Env:     map[string]string{"B": "2", "A": "it's"},
			Stdin:   []byte("stdin-data"),
			Timeout: 0,
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
		want := quoteJoin("systemd-run", "--quiet", "--collect", "--wait", "--pipe",
			"--slice="+sbx.name+".slice", "-p", "User=drill", "-p", "WorkingDirectory="+sbx.workspace,
			"--", "sh", "-c", sandbox.EnvPreludeScript(2), "sh", "psql", "-c", "SELECT 'x'")
		if got := remoteCmd(t, fake, 0); got != want {
			t.Errorf("call = %s\nwant   %s (env via stdin, everything quoted)", got, want)
		}
		// The env block precedes the request's own stdin, which reaches the
		// command untouched once the prelude has consumed exactly two lines.
		if fake.stdins[0] != "A=it's\nB=2\nstdin-data" {
			t.Errorf("stdin = %q, want the env block followed by the caller's stdin", fake.stdins[0])
		}
	})

	t.Run("no env, no env prefix", func(t *testing.T) {
		p, fake := testProvider(t, response{})
		sbx := testSandbox(p)
		if _, err := sbx.Exec(context.Background(), sandbox.ExecRequest{Argv: []string{"true"}}); err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if got := remoteCmd(t, fake, 0); strings.Contains(got, "'env'") {
			t.Errorf("call %s must not invoke env without variables", got)
		}
	})

	t.Run("timeout is honored", func(t *testing.T) {
		p, _ := testProvider(t, response{})
		sbx := testSandbox(p)
		if _, err := sbx.Exec(context.Background(), sandbox.ExecRequest{Argv: []string{"true"}, Timeout: time.Second}); err != nil {
			t.Fatalf("Exec with timeout: %v", err)
		}
	})

	t.Run("runner error surfaces", func(t *testing.T) {
		p, _ := testProvider(t, response{err: errors.New("context deadline exceeded")})
		sbx := testSandbox(p)
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

	t.Run("stream and chmod in one call", func(t *testing.T) {
		p, fake := testProvider(t, response{})
		sbx := testSandbox(p)
		res, err := sbx.PutFile(context.Background(), hostFile, sbx.ScratchDir()+"/b.dump", "")
		if err != nil {
			t.Fatalf("PutFile: %v", err)
		}
		if res.BytesCopied != 10 || res.Duration <= 0 {
			t.Errorf("result = %+v, want 10 bytes with measured duration", res)
		}
		want := quoteJoin("sh", "-c", putFileScript, "sh", sbx.ScratchDir()+"/b.dump", "0600")
		if got := remoteCmd(t, fake, 0); got != want {
			t.Errorf("call = %s\nwant   %s (default mode must be 0600)", got, want)
		}
		if fake.stdins[0] != "0123456789" {
			t.Errorf("stdin = %q, want the file bytes on stdin", fake.stdins[0])
		}
	})

	t.Run("failures", func(t *testing.T) {
		p, _ := testProvider(t)
		sbx := testSandbox(p)
		if _, err := sbx.PutFile(context.Background(), hostFile, "/x", "rw-"); !errors.Is(err, sandbox.ErrInvalidParams) {
			t.Errorf("non-octal mode: got %v, want ErrInvalidParams", err)
		}
		if _, err := sbx.PutFile(context.Background(), filepath.Join(t.TempDir(), "gone"), "/x", ""); err == nil {
			t.Error("missing host file must fail before any ssh call")
		}

		p, _ = testProvider(t, response{exit: 1, stderr: "dd: no space"})
		sbx = testSandbox(p)
		if _, err := sbx.PutFile(context.Background(), hostFile, "/x", ""); err == nil || !strings.Contains(err.Error(), "no space") {
			t.Errorf("copy failure: got %v", err)
		}

		p, _ = testProvider(t, response{err: errors.New("spawn failed")})
		sbx = testSandbox(p)
		if _, err := sbx.PutFile(context.Background(), hostFile, "/x", ""); err == nil {
			t.Error("runner error must surface")
		}
	})
}

func TestDestroy(t *testing.T) {
	t.Run("single idempotent script", func(t *testing.T) {
		p, fake := testProvider(t, response{}, response{})
		sbx := testSandbox(p)
		if err := sbx.Destroy(context.Background()); err != nil {
			t.Errorf("Destroy: %v", err)
		}
		want := quoteJoin("sh", "-c", destroyScript, "sh",
			sbx.name+".slice", sbx.name+"-reaper.timer", sbx.workspace)
		if got := remoteCmd(t, fake, 0); got != want {
			t.Errorf("call = %s\nwant   %s", got, want)
		}
		// The guards make a second Destroy the same successful call.
		if err := sbx.Destroy(context.Background()); err != nil {
			t.Errorf("Destroy of a removed sandbox must succeed (idempotent), got: %v", err)
		}
	})

	t.Run("genuine failure surfaces", func(t *testing.T) {
		p, _ := testProvider(t, response{exit: 1, stderr: "rm: permission denied"})
		sbx := testSandbox(p)
		if err := sbx.Destroy(context.Background()); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("Destroy: %v, want the remote failure", err)
		}
		p, _ = testProvider(t, response{err: errors.New("spawn failed")})
		sbx = testSandbox(p)
		if err := sbx.Destroy(context.Background()); err == nil {
			t.Error("runner error must surface")
		}
	})
}

func TestSweepOrphans(t *testing.T) {
	livePID := strconv.Itoa(os.Getpid())
	p, fake := testProvider(t,
		response{stdout: "probavi-sbx-dead1\nprobavi-sbx-live2\nprobavi-sbx-foreign3\nprobavi-sbx-lost4\nprobavi-sbx-gone5\nprobavi-sbx-junk6\nprobavi-sbx-junk7\nunrelated-dir\n"},
		response{stdout: testHostID + " 999999999\n"}, // dead1: mine, pid long gone
		response{}, // destroy dead1
		response{stdout: testHostID + " " + livePID + "\n"}, // live2: mine, this process
		// foreign3 belongs to another drill host sharing this target; its
		// pid means nothing here — never sweep, even though it is dead
		// locally.
		response{stdout: "ffff0000ffff0000 999999999\n"},
		response{stdout: "MISSING\n"}, // lost4: marker gone — ownership metadata lost
		response{},                    // destroy lost4
		// gone5 was destroyed by a concurrent drill between list and read —
		// skip, not an error and not a sweep.
		response{stdout: "GONE\n"},
		response{stdout: "not a marker at all\n"}, // junk6: malformed marker
		response{},                              // destroy junk6
		response{stdout: testHostID + " nan\n"}, // junk7: unparseable pid
		response{},                              // destroy junk7
	)
	removed, err := p.SweepOrphans(context.Background())
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if !slices.Equal(removed, []string{"probavi-sbx-dead1", "probavi-sbx-lost4", "probavi-sbx-junk6", "probavi-sbx-junk7"}) {
		t.Errorf("removed = %v, want [dead1 lost4 junk6 junk7] — live-owner and foreign-host sandboxes must survive", removed)
	}
	for i := range fake.calls {
		cmd := remoteCmd(t, fake, i)
		if strings.Contains(cmd, shQuote(destroyScript)) && (strings.Contains(cmd, "live2") || strings.Contains(cmd, "foreign3") || strings.Contains(cmd, "gone5")) {
			t.Errorf("sweep destroyed a live, foreign, or vanished sandbox: %s", cmd)
		}
		if strings.Contains(cmd, "unrelated-dir") {
			t.Errorf("sweep touched a directory without the sandbox prefix: %s", cmd)
		}
	}
	if got, want := remoteCmd(t, fake, 0), quoteJoin("sh", "-c", listScript, "sh", testRoot); got != want {
		t.Errorf("list call = %s\nwant      %s", got, want)
	}
}

func TestSweepOrphansEmpty(t *testing.T) {
	// A missing workspace root exits 0 with no output: nothing to sweep.
	p, _ := testProvider(t, response{})
	removed, err := p.SweepOrphans(context.Background())
	if err != nil || len(removed) != 0 {
		t.Errorf("SweepOrphans on empty root = %v, %v — want no removals, no error", removed, err)
	}
}

func TestSweepOrphansErrorPaths(t *testing.T) {
	t.Run("list failure", func(t *testing.T) {
		p, _ := testProvider(t, response{exit: 1, stderr: "ls: not permitted"})
		if _, err := p.SweepOrphans(context.Background()); err == nil {
			t.Error("list failure must surface")
		}
		p, _ = testProvider(t, response{err: errors.New("no ssh binary")})
		if _, err := p.SweepOrphans(context.Background()); err == nil {
			t.Error("list runner error must surface")
		}
	})
	t.Run("unreadable marker surfaces, never sweeps", func(t *testing.T) {
		p, fake := testProvider(t,
			response{stdout: "probavi-sbx-x\n"},
			response{exit: 1, stderr: "cat: Permission denied"},
		)
		if _, err := p.SweepOrphans(context.Background()); err == nil || !strings.Contains(err.Error(), "owner marker") {
			t.Errorf("unreadable marker: %v, want an error — destroying the unattributable kills live drills", err)
		}
		for i := range fake.calls {
			if strings.Contains(remoteCmd(t, fake, i), shQuote(destroyScript)) {
				t.Error("an unreadable marker must never lead to a destroy")
			}
		}
	})
	t.Run("marker runner error", func(t *testing.T) {
		p, _ := testProvider(t,
			response{stdout: "probavi-sbx-x\n"},
			response{err: errors.New("connection lost")},
		)
		if _, err := p.SweepOrphans(context.Background()); err == nil {
			t.Error("marker runner error must surface")
		}
	})
	t.Run("removal failure", func(t *testing.T) {
		p, _ := testProvider(t,
			response{stdout: "probavi-sbx-x\n"},
			response{stdout: testHostID + " 999999999\n"},
			response{exit: 1, stderr: "permission denied"},
		)
		if _, err := p.SweepOrphans(context.Background()); err == nil || !strings.Contains(err.Error(), "sweep orphan") {
			t.Errorf("removal failure: got %v", err)
		}
	})
}

// TestParseParamsAcceptsEveryDeclaredParam proves the published parameter
// list is one a drill config can actually use.
func TestParseParamsAcceptsEveryDeclaredParam(t *testing.T) {
	params := map[string]string{
		"workspace_root": "/srv/probavi", "memory": "512M", "cpus": "1",
	}
	for _, p := range Descriptor.Params {
		if _, ok := params[p.Name]; !ok {
			t.Fatalf("declared param %q has no sample value in this test", p.Name)
		}
	}
	set, err := parseParams(Descriptor, params)
	if err != nil {
		t.Fatalf("parseParams: %v", err)
	}
	if set.root != "/srv/probavi" {
		t.Errorf("workspace root %q", set.root)
	}
	if !slices.Contains(set.props, "MemoryMax=512M") || !slices.Contains(set.props, "CPUQuota=100%") {
		t.Errorf("slice properties %v", set.props)
	}
}

// TestParseParamsRejectsUnhandledDeclaredParam covers the defect path: a
// declared parameter parseParams never applies.
func TestParseParamsRejectsUnhandledDeclaredParam(t *testing.T) {
	d := Descriptor
	d.Params = append(append([]sandbox.Param{}, d.Params...),
		sandbox.Param{Name: "readonly", Doc: "Declared but not implemented."})
	_, err := parseParams(d, map[string]string{"readonly": "true"})
	if !errors.Is(err, sandbox.ErrInvalidParams) {
		t.Fatalf("error %v is not ErrInvalidParams", err)
	}
	if !strings.Contains(err.Error(), "declared but not implemented") {
		t.Errorf("error %q does not explain the defect", err)
	}
}

// TestSweepAsksTheOwnerLivenessCheck is the remotehost half of the
// portability fix: see the docker provider's test of the same name. The
// owner process runs on the drill host, so its liveness must be decided
// without /proc — which a macOS drill host does not have.
func TestSweepAsksTheOwnerLivenessCheck(t *testing.T) {
	tests := []struct {
		name        string
		alive       bool
		wantRemoved []string
	}{
		{"live owner is spared", true, []string{}},
		{"dead owner is swept", false, []string{"probavi-sbx-one"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			responses := []response{
				{stdout: "probavi-sbx-one\n"},
				{stdout: testHostID + " 4242\n"},
			}
			if !tt.alive {
				responses = append(responses, response{}) // destroy
			}
			p, _ := testProvider(t, responses...)
			asked := 0
			p.alive = func(id string) bool {
				asked++
				if id != "4242" {
					t.Errorf("liveness asked about %q, want the marker's 4242", id)
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
// half of the leak: env used to be passed as `env NAME=value` in the
// remote command line, so a database password sat in the process list on
// the drill host and on the target. internal/checks refuses {{password}}
// in sql_runner argv for exactly that reason.
func TestExecKeepsSecretsOutOfArgv(t *testing.T) {
	const secret = "s3cr3t-ephemeral-password"
	p, fake := testProvider(t, response{})
	sbx := testSandbox(p)

	if _, err := sbx.Exec(context.Background(), sandbox.ExecRequest{
		Argv: []string{"psql", "-c", "SELECT 1"},
		Env:  map[string]string{"PGPASSWORD": secret},
	}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if cmd := remoteCmd(t, fake, 0); strings.Contains(cmd, secret) {
		t.Fatalf("remote command carries the secret — visible in ps on both hosts: %s", cmd)
	}
	if !strings.HasPrefix(fake.stdins[0], "PGPASSWORD="+secret+"\n") {
		t.Errorf("stdin = %q, want the secret delivered out of band", fake.stdins[0])
	}
}

// TestExecTerminatesSystemdOptions pins the "--" between systemd-run's own
// options and the payload. The payload is the adapter's argv, which
// SECURITY.md names as attacker-reachable input: unterminated, "-p
// User=root" would override the drill user this provider sets (the later
// property wins) and "--slice=" would move the payload out of the slice
// Destroy stops.
func TestExecTerminatesSystemdOptions(t *testing.T) {
	optionShaped := []string{"-p", "User=root", "--slice=escape.slice", "id"}

	for name, tt := range map[string]struct {
		env  map[string]string
		tail []string
	}{
		"without env": {tail: optionShaped},
		"with env": {
			env:  map[string]string{"A": "1"},
			tail: append([]string{"sh", "-c", sandbox.EnvPreludeScript(1), "sh"}, optionShaped...),
		},
	} {
		t.Run(name, func(t *testing.T) {
			p, fake := testProvider(t, response{})
			sbx := testSandbox(p)
			if _, err := sbx.Exec(context.Background(), sandbox.ExecRequest{
				Argv: optionShaped,
				Env:  tt.env,
			}); err != nil {
				t.Fatalf("Exec: %v", err)
			}
			want := quoteJoin(append([]string{
				"systemd-run", "--quiet", "--collect", "--wait", "--pipe",
				"--slice=" + sbx.name + ".slice", "-p", "User=drill",
				"-p", "WorkingDirectory=" + sbx.workspace, "--",
			}, tt.tail...)...)
			if got := remoteCmd(t, fake, 0); got != want {
				t.Errorf("call = %s\nwant   %s (every argv element after the terminator)", got, want)
			}
		})
	}
}

// TestExecRejectsUnexpressibleEnv covers the one value a line protocol
// cannot carry. Truncating a credential silently — or exporting its tail
// as another variable — would be worse than refusing it.
func TestExecRejectsUnexpressibleEnv(t *testing.T) {
	p, fake := testProvider(t)
	sbx := testSandbox(p)

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
