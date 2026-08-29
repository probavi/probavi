// Package remotehost implements the Probavi sandbox provider for dedicated
// drill hosts without any container runtime: one sandbox is one transient
// systemd slice plus one per-drill workspace directory on the target,
// driven exclusively through the OpenSSH client binary (deliberately not a
// Go SSH library: the operator's ssh config, agent, and known_hosts apply
// unchanged, and host key verification is never disabled by the provider).
// The design spec is docs/sandbox-bare-host.md; use this provider only
// when the target cannot run a container runtime at all — the default
// answer for remote drills stays the docker provider over
// DOCKER_HOST=ssh://…, where every container guarantee holds unchanged.
//
// Every command a sandbox ever runs — provider verbs and the
// adapter-started engine alike — executes as a transient unit inside the
// sandbox's slice (systemd-run --slice), so resource caps bound the sum of
// everything in the sandbox and stopping the slice kills the whole process
// tree, however it was started. Cleanup is layered like the k8s provider:
// Destroy stops the slice and removes the workspace; SweepOrphans reaps
// workspaces whose creating process on THIS drill host is gone
// (host-scoped through the owner marker, so several drill hosts may share
// one target — connecting as the same remote user, since markers live in
// 0700 workspaces); and a target-side transient timer armed at Create
// stops the slice and removes the workspace after a hard deadline even if
// the drill host never comes back.
//
// The ssh target (user@host) is connection detail: it lives in the
// PROBAVI_SSH_TARGET environment variable, never in drill config — sandbox
// params are recorded verbatim in evidence records, and connection details
// must never appear there (evidence-schema.md §8). The spec's
// non-negotiable operational premise applies: the target host is dedicated
// to drills, and restored production data touches its persistent disks
// until Destroy deletes (not shreds) the workspace.
package remotehost

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/probavi/probavi/internal/sandbox"
	"github.com/probavi/probavi/internal/sandbox/cli"
)

// EnvTarget names the environment variable selecting the ssh target
// (user@host). Environment-only by design — see the package comment.
const EnvTarget = "PROBAVI_SSH_TARGET"

const (
	// namePrefix starts every slice, unit, and workspace name this provider
	// creates; the orphan sweep matches on it. Never remove anything on the
	// target that does not carry it.
	namePrefix = "probavi-sbx-"

	// defaultWorkspaceRoot holds per-drill workspaces unless the
	// workspace_root param overrides it; the operator creates it owned by
	// the drill user during host setup (README "Bare-host drills").
	defaultWorkspaceRoot = "/var/lib/probavi-drills"

	// minSystemdVersion is the spec's floor (docs/sandbox-bare-host.md §3,
	// decided 2026-08-01), probed at first contact.
	minSystemdVersion = 244

	// hardDeadlineSeconds arms the target-side cleanup backstop: a
	// transient timer stops the slice and removes the workspace this long
	// after Create, so production data does not outlive a vanished drill
	// host. Mirrors the k8s provider's activeDeadlineSeconds.
	hardDeadlineSeconds = 7200
)

// Shell fragments executed on the target through `sh -c <script> sh
// <args…>`: arguments travel as positional parameters, never interpolated
// into the script, so paths cannot be re-interpreted by the remote shell.
const (
	// setupScript creates the workspace with its scratch dir and records
	// the owner marker ("<host-id> <pid>") for the sweep. One invocation,
	// so a crash cannot leave a workspace without at least attempting the
	// marker; a workspace that still ends up markerless is swept as an
	// orphan.
	setupScript = `mkdir -p "$1/scratch" && chmod 700 "$1" && printf '%s\n' "$2" > "$1/owner"`

	// destroyScript is the single idempotent teardown: stop the slice
	// (kills every descendant), disarm the deadline timer, remove the
	// workspace. The is-active guards make a missing slice or timer
	// success, locale-independently; rm -rf of a missing workspace already
	// is.
	destroyScript = `set -e; if systemctl --quiet is-active -- "$1"; then systemctl stop -- "$1"; fi; if systemctl --quiet is-active -- "$2"; then systemctl stop -- "$2"; fi; rm -rf -- "$3"`

	// listScript lists workspace names for the sweep; a missing root means
	// nothing to sweep, not an error.
	listScript = `[ -d "$1" ] || exit 0; ls -1 -- "$1"`

	// ownerScript reads a workspace's owner marker for the sweep. The
	// sentinels are distinguishable from real markers (a host id is 16 hex
	// chars): GONE — the workspace vanished between list and read (a
	// concurrent destroy finished first); MISSING — the workspace exists
	// but lost its ownership metadata.
	ownerScript = `if [ ! -d "$1" ]; then echo GONE; elif [ ! -e "$1/owner" ]; then echo MISSING; else cat -- "$1/owner"; fi`

	// putFileScript streams stdin into the destination and applies the
	// mode — the k8s provider's positional trick; bytes cross only the ssh
	// connection. The brief pre-chmod window is harmless: the enclosing
	// workspace is 0700.
	putFileScript = `cat > "$1" && chmod "$2" "$1"`
)

var memoryPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?[KMGTPE]?$`)

// Descriptor is this provider's self-description: the parameter gate
// parseParams resolves every configured key through, and the source the
// generated capabilities manifest reads. There is no image param; its
// absence is what this provider is.
var Descriptor = sandbox.Descriptor{
	ID:     "remotehost",
	Name:   "Bare host over SSH",
	Status: "experimental",
	Params: []sandbox.Param{
		{Name: "workspace_root", Default: defaultWorkspaceRoot, Doc: "Absolute path on the target holding per-drill workspaces."},
		{Name: "memory", Doc: "MemoryMax of the drill's transient systemd slice (systemd size suffixes)."},
		{Name: "cpus", Doc: "CPUQuota of the drill's transient systemd slice (decimal CPU count)."},
	},
	Isolation: sandbox.Isolation{
		PublishedPorts: false,
		Storage:        "per-drill workspace under the workspace root, mode 0700, deleted (not shredded) at teardown",
		ForcedTeardown: true,
		OrphanSweep:    "host-scoped owner markers under the workspace root, swept at drill start",
		ExternalBackstop: fmt.Sprintf(
			"target-side transient systemd timer stops the slice and removes the workspace %ds after creation, so restored data does not outlive a vanished drill host",
			hardDeadlineSeconds),
	},
	Constraints: []string{
		"No container isolation. A dedicated drill host is a premise of this provider, not a recommendation.",
		fmt.Sprintf("systemd %d or newer on the target, probed at first contact.", minSystemdVersion),
		"Engine and tool versions are whatever the target host has installed; the version match a sandbox image guarantees does not apply here.",
		"Requires the right to run transient systemd units as the drill user — the polkit rule the README ships.",
		"Per-command environment values reach the command through stdin, never the remote command line: ssh has no environment channel independent of the target's AcceptEnv, and a value in argv would be readable from the process list on both hosts.",
		"The target is named by the " + EnvTarget + " environment variable only, never in drill config: sandbox params are recorded verbatim in signed evidence, and connection details must never appear there.",
	},
	VerifiedAgainst: []string{"systemd host over the OpenSSH CLI (CI integration suite)"},
	Docs:            "docs/sandbox-bare-host.md",
}

// Provider creates and destroys bare-host sandboxes on one ssh target.
type Provider struct {
	bin    string
	run    cli.Runner
	logger *slog.Logger
	pid    int
	hostID string

	target        string
	workspaceRoot string

	// alive reports whether the process a sandbox's owner id names still
	// runs. Injected so the sweep's decision can be tested without spawning
	// processes.
	alive func(ownerID string) bool
}

// New returns a Provider for the target named by PROBAVI_SSH_TARGET. The
// drill's sandbox params are validated here too, so a misconfigured drill
// fails before any ssh connection: SweepOrphans needs the workspace root
// ahead of Create.
func New(logger *slog.Logger, params map[string]string) (*Provider, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	target := os.Getenv(EnvTarget)
	if target == "" {
		return nil, fmt.Errorf("remotehost provider: %s must be set (user@host of the dedicated drill target; environment-only by design, docs/sandbox-bare-host.md §6)", EnvTarget)
	}
	if strings.HasPrefix(target, "-") {
		return nil, fmt.Errorf("remotehost provider: %s %q must not begin with %q — it would be read as an ssh option", EnvTarget, target, "-")
	}
	set, err := parseParams(Descriptor, params)
	if err != nil {
		return nil, err
	}
	return &Provider{
		bin:           "ssh",
		run:           cli.ExecRunner{},
		logger:        logger,
		pid:           os.Getpid(),
		hostID:        sandbox.HostID(),
		target:        target,
		workspaceRoot: set.root,
		alive:         sandbox.OwnerAlive,
	}, nil
}

// Sandbox is one transient slice plus one workspace on the target.
type Sandbox struct {
	name      string // probavi-sbx-<suffix>: slice, units, and directory share it
	workspace string
	user      string // remote user every payload runs as (-p User=)
	p         *Provider
}

// settings is the validated form of the drill-config sandbox params.
type settings struct {
	root  string
	props []string // systemctl set-property arguments for the slice
}

// parseParams validates drill-config sandbox params. The accepted keys are
// exactly what the descriptor declares — anything else is an error, because
// a typo must not silently weaken a sandbox — and a declared key this
// function does not implement is an error too. The descriptor is a
// parameter so tests can drive both failure paths.
func parseParams(d sandbox.Descriptor, params map[string]string) (*settings, error) {
	set := &settings{root: defaultWorkspaceRoot}
	for _, k := range sortedKeys(params) {
		v := params[k]
		spec, ok := d.Lookup(k)
		if !ok {
			return nil, d.UnknownParamError(k)
		}
		switch spec.Name {
		case "workspace_root":
			if !strings.HasPrefix(v, "/") {
				return nil, fmt.Errorf("%w: workspace_root %q must be an absolute path", sandbox.ErrInvalidParams, v)
			}
			set.root = path.Clean(v)
		case "memory":
			if !memoryPattern.MatchString(v) {
				return nil, fmt.Errorf("%w: memory %q is not a systemd size (bytes or K/M/G/T suffix)", sandbox.ErrInvalidParams, v)
			}
			set.props = append(set.props, "MemoryMax="+v)
		case "cpus":
			f, err := strconv.ParseFloat(v, 64)
			if err != nil || f <= 0 {
				return nil, fmt.Errorf("%w: cpus %q is not a positive CPU count", sandbox.ErrInvalidParams, v)
			}
			set.props = append(set.props, fmt.Sprintf("CPUQuota=%d%%", int(math.Round(f*100))))
		default:
			return nil, d.UnhandledParamError(k)
		}
	}
	return set, nil
}

// Create probes the target (systemd version, remote user), establishes the
// workspace with its owner marker, verifies transient-unit rights with the
// exact shape Exec will use, applies resource caps to the slice, and arms
// the deadline backstop. There is no container entrypoint: the adapter
// starts and owns the engine through exec verbs (the idle pattern).
func (p *Provider) Create(ctx context.Context, params map[string]string) (*Sandbox, error) {
	set, err := parseParams(Descriptor, params)
	if err != nil {
		return nil, err
	}
	if err := p.probeSystemd(ctx); err != nil {
		return nil, err
	}
	user, err := p.remoteUser(ctx)
	if err != nil {
		return nil, err
	}
	name := namePrefix + randomSuffix()
	sbx := &Sandbox{name: name, workspace: path.Join(set.root, name), user: user, p: p}
	if err := p.setup(ctx, sbx, set); err != nil {
		// Cleanup on the failure path runs on a fresh context: the caller's
		// context may already be dead.
		dctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if derr := sbx.Destroy(dctx); derr != nil {
			p.logger.Error("destroy after failed create", "id", name, "err", derr)
		}
		return nil, err
	}
	p.logger.Info("sandbox created", "id", name, "workspace", sbx.workspace)
	return sbx, nil
}

// probeSystemd refuses targets below the spec's systemd floor at first
// contact, with a clear error instead of an obscure mid-drill failure.
func (p *Provider) probeSystemd(ctx context.Context) error {
	stdout, stderr, _, exit, err := p.ssh(ctx, nil, "systemctl", "--version")
	if err != nil {
		return fmt.Errorf("probe target systemd: %w", err)
	}
	if exit != 0 {
		return fmt.Errorf("probe target systemd: systemctl exited %d: %s", exit, firstLine(stderr))
	}
	fields := strings.Fields(firstLine(stdout))
	if len(fields) < 2 || fields[0] != "systemd" {
		return fmt.Errorf("probe target systemd: cannot parse version from %q", firstLine(stdout))
	}
	version, err := strconv.Atoi(fields[1])
	if err != nil {
		return fmt.Errorf("probe target systemd: cannot parse version from %q", firstLine(stdout))
	}
	if version < minSystemdVersion {
		return fmt.Errorf("target systemd %d is older than the required %d (docs/sandbox-bare-host.md §3)", version, minSystemdVersion)
	}
	return nil
}

// remoteUser resolves the user payloads run as: system-level transient
// units default to root, and a restored engine must never run as root.
func (p *Provider) remoteUser(ctx context.Context) (string, error) {
	stdout, stderr, _, exit, err := p.ssh(ctx, nil, "id", "-un")
	if err != nil {
		return "", fmt.Errorf("resolve remote user: %w", err)
	}
	if exit != 0 {
		return "", fmt.Errorf("resolve remote user: id exited %d: %s", exit, firstLine(stderr))
	}
	user := strings.TrimSpace(string(stdout))
	if user == "" {
		return "", fmt.Errorf("resolve remote user: id -un returned nothing")
	}
	return user, nil
}

// setup runs the target-side half of Create; any failure destroys the
// partial sandbox.
func (p *Provider) setup(ctx context.Context, sbx *Sandbox, set *settings) error {
	marker := p.hostID + " " + sandbox.OwnerID(p.pid)
	if _, stderr, _, exit, err := p.ssh(ctx, nil,
		"sh", "-c", setupScript, "sh", sbx.workspace, marker); err != nil {
		return fmt.Errorf("create workspace: %w", err)
	} else if exit != 0 {
		return fmt.Errorf("create workspace: exited %d: %s", exit, firstLine(stderr))
	}
	// Verify transient-unit rights with the exact shape Exec will use, so
	// a polkit misconfiguration fails here, not mid-restore.
	if _, stderr, _, exit, err := p.ssh(ctx, nil,
		append(sbx.execPrefix(), "true")...); err != nil {
		return fmt.Errorf("verify transient unit rights: %w", err)
	} else if exit != 0 {
		return fmt.Errorf("verify transient unit rights: systemd-run exited %d: %s (is the polkit rule from README.md installed for the drill user?)", exit, firstLine(stderr))
	}
	// Caps sit on the slice, so they bound the sum of everything in the
	// sandbox, exactly like a container's cgroup.
	if len(set.props) > 0 {
		args := append([]string{"systemctl", "set-property", "--runtime", sbx.slice()}, set.props...)
		if _, stderr, _, exit, err := p.ssh(ctx, nil, args...); err != nil {
			return fmt.Errorf("apply resource caps: %w", err)
		} else if exit != 0 {
			return fmt.Errorf("apply resource caps: set-property exited %d: %s", exit, firstLine(stderr))
		}
	}
	// The deadline backstop runs without User=: our own fixed command, and
	// root survives any permission oddity a half-dead drill leaves behind.
	reaper := "systemctl stop " + shQuote(sbx.slice()) + "; rm -rf " + shQuote(sbx.workspace)
	if _, stderr, _, exit, err := p.ssh(ctx, nil,
		"systemd-run", "--quiet", "--collect", "--unit="+sbx.reaperUnit(),
		"--on-active="+strconv.Itoa(hardDeadlineSeconds), "--timer-property=AccuracySec=1m",
		"sh", "-c", reaper); err != nil {
		return fmt.Errorf("arm deadline backstop: %w", err)
	} else if exit != 0 {
		return fmt.Errorf("arm deadline backstop: systemd-run exited %d: %s", exit, firstLine(stderr))
	}
	return nil
}

// SweepOrphans removes workspaces (with their slices and timers) created
// on this drill host by processes that no longer run. Live processes'
// sandboxes are kept; other hosts' sandboxes are never touched — their
// markers name a foreign host id, and their deadline timers back them up.
// A markerless workspace carries our name prefix but lost its ownership
// metadata: swept. Returns the removed sandbox names.
func (p *Provider) SweepOrphans(ctx context.Context) ([]string, error) {
	stdout, stderr, _, exit, err := p.ssh(ctx, nil, "sh", "-c", listScript, "sh", p.workspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("list sandbox workspaces: %w", err)
	}
	if exit != 0 {
		return nil, fmt.Errorf("list sandbox workspaces: exited %d: %s", exit, firstLine(stderr))
	}
	names := strings.Split(string(stdout), "\n")
	removed := make([]string, 0, len(names))
	for _, name := range names {
		if !strings.HasPrefix(name, namePrefix) {
			continue
		}
		orphan, err := p.isOrphan(ctx, name)
		if err != nil {
			return removed, err
		}
		if !orphan {
			continue
		}
		if err := p.destroy(ctx, name, path.Join(p.workspaceRoot, name)); err != nil {
			return removed, fmt.Errorf("sweep orphan %s: %w", name, err)
		}
		p.logger.Info("swept orphan sandbox", "id", name)
		removed = append(removed, name)
	}
	return removed, nil
}

// isOrphan reports whether a workspace's owner process on this drill host
// is gone. An unreadable-but-present marker surfaces as an error rather
// than a verdict: destroying what we cannot attribute is how a sweep kills
// someone else's live drill.
func (p *Provider) isOrphan(ctx context.Context, name string) (bool, error) {
	workspace := path.Join(p.workspaceRoot, name)
	stdout, stderr, _, exit, err := p.ssh(ctx, nil, "sh", "-c", ownerScript, "sh", workspace)
	if err != nil {
		return false, fmt.Errorf("read owner marker of %s: %w", name, err)
	}
	if exit != 0 {
		return false, fmt.Errorf("read owner marker of %s: exited %d: %s", name, exit, firstLine(stderr))
	}
	switch marker := strings.TrimSpace(string(stdout)); marker {
	case "GONE":
		// Vanished between list and read: a concurrent destroy finished
		// first. Gone means nothing left to sweep — not an error.
		return false, nil
	case "MISSING":
		return true, nil
	default:
		fields := strings.Fields(marker)
		if len(fields) != 2 {
			return true, nil // malformed marker: ownership metadata is gone
		}
		if fields[0] != p.hostID {
			return false, nil // another drill host's sandbox: never ours to sweep
		}
		return !p.alive(fields[1]), nil
	}
}

// ID returns the sandbox name (slice, units, and workspace share it).
func (s *Sandbox) ID() string { return s.name }

// ScratchDir returns the writable directory guaranteed inside the sandbox
// (adapter protocol §6.2 sandbox.scratch_dir).
func (s *Sandbox) ScratchDir() string { return s.workspace + "/scratch" }

func (s *Sandbox) slice() string      { return s.name + ".slice" }
func (s *Sandbox) reaperUnit() string { return s.name + "-reaper" }

// execPrefix is the transient-unit shape every payload runs with: inside
// the slice, as the drill user, in the workspace, stdio piped through the
// ssh connection.
//
// It ends in the "--" terminator, and that is load-bearing rather than
// tidy: systemd-run keeps parsing its own options until it sees one, and
// the payload that follows is an adapter's argv — attacker-reachable
// input (SECURITY.md). Unterminated, an argv beginning "-p User=root"
// would set a property of the unit instead of an argument of the command
// (the later property wins), and "--slice=" would move the payload out of
// the slice Destroy stops. The k8s provider terminates for the same
// reason; the docker CLI refuses such argv on its own.
func (s *Sandbox) execPrefix() []string {
	return []string{
		"systemd-run", "--quiet", "--collect", "--wait", "--pipe",
		"--slice=" + s.slice(),
		"-p", "User=" + s.user,
		"-p", "WorkingDirectory=" + s.workspace,
		"--",
	}
}

// Exec runs one command inside the sandbox slice (adapter protocol §4.1).
// Per-command environment is applied through env(1), same as the k8s
// provider; values are visible in the target's process list for the
// command's duration — the spec accepts this on a dedicated host only
// (docs/sandbox-bare-host.md §6). A command cut off by the context (the
// local ssh dies) may leave its remote unit running; the slice bounds it,
// and Destroy or the deadline backstop reaps it.
func (s *Sandbox) Exec(ctx context.Context, req sandbox.ExecRequest) (*sandbox.ExecResult, error) {
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	stdin := string(req.Stdin)
	args := s.execPrefix()
	if len(req.Env) > 0 {
		// ssh has no out-of-band environment channel that does not depend on
		// the target's AcceptEnv, so the values reach the command through
		// stdin rather than the remote command line, where `ps` would show
		// them on the drill host and on the target alike.
		lines, err := sandbox.EnvPreludeLines(req.Env)
		if err != nil {
			return nil, err
		}
		stdin = lines + stdin
		args = append(args, "sh", "-c", sandbox.EnvPreludeScript(len(req.Env)), "sh")
	}
	args = append(args, req.Argv...)

	start := time.Now()
	stdout, stderr, truncated, exit, err := s.p.ssh(ctx, strings.NewReader(stdin), args...)
	if err != nil {
		return nil, fmt.Errorf("exec in sandbox %s: %w", s.name, err)
	}
	return &sandbox.ExecResult{
		ExitCode:  exit,
		Stdout:    stdout,
		Stderr:    stderr,
		Truncated: truncated,
		Duration:  time.Since(start),
	}, nil
}

// PutFile streams a host file into the sandbox workspace and applies mode
// (octal string, default "0600") — adapter protocol §4.2. Bytes cross only
// the ssh connection. Path allow-listing is the core's responsibility; the
// provider only moves bytes.
func (s *Sandbox) PutFile(ctx context.Context, hostPath, destPath, mode string) (*sandbox.PutFileResult, error) {
	if mode == "" {
		mode = "0600"
	}
	if _, err := strconv.ParseUint(mode, 8, 32); err != nil {
		return nil, fmt.Errorf("%w: mode %q is not octal", sandbox.ErrInvalidParams, mode)
	}
	f, err := os.Open(hostPath)
	if err != nil {
		return nil, fmt.Errorf("put_file source: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only descriptor
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("put_file source: %w", err)
	}

	start := time.Now()
	if _, stderr, _, exit, err := s.p.ssh(ctx, f, "sh", "-c", putFileScript, "sh", destPath, mode); err != nil {
		return nil, fmt.Errorf("copy into sandbox %s: %w", s.name, err)
	} else if exit != 0 {
		return nil, fmt.Errorf("copy into sandbox %s: exited %d: %s", s.name, exit, firstLine(stderr))
	}
	return &sandbox.PutFileResult{BytesCopied: info.Size(), Duration: time.Since(start)}, nil
}

// Destroy stops the slice (kills every descendant, however it was
// started), disarms the deadline timer, and removes the workspace. It is
// idempotent: destroying an already-removed sandbox succeeds.
func (s *Sandbox) Destroy(ctx context.Context) error {
	if err := s.p.destroy(ctx, s.name, s.workspace); err != nil {
		return fmt.Errorf("destroy sandbox: %w", err)
	}
	s.p.logger.Info("sandbox destroyed", "id", s.name)
	return nil
}

func (p *Provider) destroy(ctx context.Context, name, workspace string) error {
	_, stderr, _, exit, err := p.ssh(ctx, nil,
		"sh", "-c", destroyScript, "sh", name+".slice", name+"-reaper.timer", workspace)
	if err != nil {
		return err
	}
	if exit != 0 {
		return fmt.Errorf("remote cleanup exited %d: %s", exit, firstLine(stderr))
	}
	return nil
}

// ssh runs one command on the target. The OpenSSH client hands its command
// arguments to the remote login shell as a single string, so every
// argument is single-quoted first — adapter-supplied env values and
// operator paths are never re-interpreted by that shell. BatchMode keeps
// automation from hanging on a prompt; host key verification stays
// OpenSSH's, against the operator's known_hosts, never disabled.
func (p *Provider) ssh(ctx context.Context, stdin io.Reader, argv ...string) ([]byte, []byte, bool, int, error) {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = shQuote(a)
	}
	return p.run.Run(ctx, stdin, nil, p.bin, "-o", "BatchMode=yes", p.target, strings.Join(quoted, " "))
}

// shQuote wraps s in single quotes for the remote POSIX shell, closing and
// reopening around embedded single quotes.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func randomSuffix() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is unrecoverable; the name is cosmetic for
		// uniqueness only — fall back to the pid.
		return "p" + strconv.Itoa(os.Getpid())
	}
	return hex.EncodeToString(b[:])
}
