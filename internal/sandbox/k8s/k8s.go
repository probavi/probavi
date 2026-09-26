// Package k8s implements the Probavi sandbox provider on Kubernetes: every
// sandbox is a batch/v1 Job running exactly one pod, driven through the
// kubectl CLI (deliberately not client-go: the CLI is a boring, already
// verified dependency of the operator's host, and the SDK would drag a
// large module tree into a trust product). Cluster selection follows
// kubectl's own rules (KUBECONFIG, current context).
//
// Security defaults (AGENTS.md §3.3): no ports are declared and no Service
// is ever created; the pod never mounts a service-account token; restored
// sandboxes contain production data, so restrict in-cluster reachability
// with a NetworkPolicy matching the com.probavi.sandbox=1 pod label —
// unlike the docker provider's --network none, pod-level network isolation
// is the cluster's job, not expressible by a workload alone. Document this
// residual risk to operators.
//
// Cleanup is layered: Destroy deletes the Job with foreground cascading;
// SweepOrphans removes Jobs whose creating process on THIS host is gone;
// and the Job spec itself carries activeDeadlineSeconds plus
// ttlSecondsAfterFinished, so the cluster kills and garbage-collects a
// sandbox even if the probavi host dies and never returns — a backstop no
// host-local provider can offer.
package k8s

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/probavi/probavi/internal/sandbox"
	"github.com/probavi/probavi/internal/sandbox/cli"
)

const (
	// LabelSandbox marks every Job and pod this provider creates; the orphan
	// sweep and the operator's NetworkPolicy match on it.
	LabelSandbox = "com.probavi.sandbox"
	// labelPID records the creating process for the orphan sweep.
	labelPID = "com.probavi.pid"
	// labelHost scopes the sweep: process liveness is only checkable on the
	// host that created the sandbox, so the sweep skips other hosts' Jobs
	// and leaves them to their cluster-side deadlines.
	labelHost = "com.probavi.host"

	scratchDir    = "/tmp"
	awaitInterval = 500 * time.Millisecond
	// maxAwaitRunning exceeds the docker provider's cap because the image
	// pull happens after Job creation, inside the cluster.
	maxAwaitRunning = 5 * time.Minute

	// activeDeadlineSeconds and ttlSecondsAfterFinished are the cluster-side
	// cleanup backstop: after the deadline the kubelet kills the pod, the
	// Job finishes as failed, and the TTL controller deletes both objects —
	// production data does not outlive a crashed drill host.
	activeDeadlineSeconds   = 7200
	ttlSecondsAfterFinished = 600
)

// Descriptor is this provider's self-description: the parameter gate
// manifest resolves every configured key through, and the source the
// generated capabilities manifest reads.
var Descriptor = sandbox.Descriptor{
	ID:     "k8s",
	Name:   "Kubernetes Job",
	Status: "experimental",
	Params: []sandbox.Param{
		{Name: "image", Required: true, Doc: "Sandbox image; it must contain the engine and the restore tooling the adapter drives."},
		{Name: "namespace", Default: "default", Doc: "Namespace the Job is created in."},
		{Name: "memory", Doc: "Container memory request and limit, set equal so the pod is guaranteed."},
		{Name: "cpus", Doc: "Container CPU request and limit, set equal so the pod is guaranteed."},
		{Name: "command", Doc: "Container command override, split on whitespace — for engines the adapter starts itself."},
		{Name: "env.", Family: true, Doc: "Environment variable set inside the sandbox."},
	},
	Isolation: sandbox.Isolation{
		PublishedPorts: false,
		Storage:        "pod filesystem, deleted with the Job",
		ForcedTeardown: true,
		OrphanSweep:    "host- and pid-scoped labels, swept at drill start",
		ExternalBackstop: fmt.Sprintf(
			"cluster-side: activeDeadlineSeconds %d then TTL %ds, so a sandbox dies even if the drill host does",
			activeDeadlineSeconds, ttlSecondsAfterFinished),
	},
	Constraints: []string{
		"Requires a working kubectl context with rights to create, exec into, and delete Jobs in the target namespace; the kubectl CLI is driven directly, never client-go.",
		"Network isolation is the cluster's job: the pod carries the sandbox label so a NetworkPolicy can select it. There is no pod-level equivalent of the docker provider's --network none.",
		"The pod runs with no service-account token, no service links, and the RuntimeDefault seccomp profile.",
		"Per-command environment values reach the command through stdin, never the command line: kubectl exec has no environment flag, and a value in argv would be readable from the process list on the drill host and inside the pod. Requires sh in the image, as put_file already does.",
	},
	VerifiedAgainst: []string{"kind cluster (CI integration suite)"},
}

// Provider creates and destroys Kubernetes-backed sandboxes.
type Provider struct {
	bin    string
	run    cli.Runner
	logger *slog.Logger
	pid    int
	hostID string

	awaitInterval time.Duration
	awaitCap      time.Duration

	// alive reports whether the process a sandbox's owner id names still
	// runs. Injected so the sweep's decision can be tested without spawning
	// processes.
	alive func(ownerID string) bool
}

// New returns a Provider shelling out to the "kubectl" binary.
func New(logger *slog.Logger) *Provider {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Provider{
		bin:           "kubectl",
		run:           cli.ExecRunner{},
		logger:        logger,
		pid:           os.Getpid(),
		hostID:        sandbox.HostID(),
		awaitInterval: awaitInterval,
		awaitCap:      maxAwaitRunning,
		alive:         sandbox.OwnerAlive,
	}
}

// Sandbox is one running disposable Job with its single pod.
type Sandbox struct {
	job       string
	pod       string
	namespace string
	p         *Provider
}

// Create submits a Job built from drill-config sandbox params and waits
// until its pod runs. Engine readiness inside the pod is the adapter's job.
func (p *Provider) Create(ctx context.Context, params map[string]string) (*Sandbox, error) {
	m, namespace, err := p.manifest(Descriptor, params)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encode job manifest: %w", err)
	}
	_, stderr, _, exit, err := p.run.Run(ctx, strings.NewReader(string(raw)), nil,
		p.bin, "create", "-n", namespace, "-f", "-")
	if err != nil {
		return nil, fmt.Errorf("create sandbox: %w", err)
	}
	if exit != 0 {
		return nil, fmt.Errorf("create sandbox: kubectl create exited %d: %s", exit, firstLine(stderr))
	}
	sbx := &Sandbox{job: m.Metadata.Name, namespace: namespace, p: p}
	if err := p.awaitRunning(ctx, sbx); err != nil {
		// Cleanup on the failure path runs on a fresh context: the caller's
		// context may already be dead.
		dctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if derr := sbx.Destroy(dctx); derr != nil {
			p.logger.Error("destroy after failed create", "id", sbx.ID(), "err", derr)
		}
		return nil, err
	}
	p.logger.Info("sandbox created", "id", sbx.ID(), "pod", sbx.pod)
	return sbx, nil
}

// SweepOrphans removes labeled Jobs created on this host by processes that
// no longer run. Jobs of live processes (concurrent drills) are kept; Jobs
// from other hosts are left to their owners and to the cluster-side
// deadlines, because process liveness is only checkable locally. Returns
// the removed ids as namespace/name.
func (p *Provider) SweepOrphans(ctx context.Context) ([]string, error) {
	stdout, stderr, _, exit, err := p.run.Run(ctx, nil, nil, p.bin,
		"get", "jobs", "--all-namespaces", "-l", LabelSandbox+"=1", "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("list sandbox jobs: %w", err)
	}
	if exit != 0 {
		return nil, fmt.Errorf("list sandbox jobs: kubectl get exited %d: %s", exit, firstLine(stderr))
	}
	var list jobList
	if err := json.Unmarshal(stdout, &list); err != nil {
		return nil, fmt.Errorf("parse sandbox job list: %w", err)
	}
	removed := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		if !p.isOrphan(item.Metadata.Labels) {
			continue
		}
		id := item.Metadata.Namespace + "/" + item.Metadata.Name
		if err := p.remove(ctx, item.Metadata.Namespace, item.Metadata.Name); err != nil {
			return removed, fmt.Errorf("sweep orphan %s: %w", id, err)
		}
		p.logger.Info("swept orphan sandbox", "id", id)
		removed = append(removed, id)
	}
	return removed, nil
}

// isOrphan reports whether a Job's owner process on this host is gone. A
// missing or malformed pid label counts as orphaned: the Job carries our
// label but lost its ownership metadata. Jobs from other hosts are never
// orphans here.
func (p *Provider) isOrphan(labels map[string]string) bool {
	if labels[labelHost] != p.hostID {
		return false
	}
	return !p.alive(labels[labelPID])
}

// ID returns namespace/job-name.
func (s *Sandbox) ID() string { return s.namespace + "/" + s.job }

// ScratchDir returns the writable directory guaranteed inside the sandbox
// (adapter protocol §6.2 sandbox.scratch_dir).
func (s *Sandbox) ScratchDir() string { return scratchDir }

// Exec runs one command inside the sandbox pod (adapter protocol §4.1).
// Per-command environment is applied through env(1), which every mainstream
// database image ships; kubectl exec itself cannot set variables.
func (s *Sandbox) Exec(ctx context.Context, req sandbox.ExecRequest) (*sandbox.ExecResult, error) {
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	stdin := string(req.Stdin)
	args := []string{"exec", "-n", s.namespace}
	if len(req.Stdin) > 0 || len(req.Env) > 0 {
		args = append(args, "-i")
	}
	args = append(args, s.pod, "--")
	if len(req.Env) > 0 {
		// kubectl exec has no environment flag, so the values reach the
		// command through stdin rather than the command line, where `ps`
		// would show them on the drill host and inside the pod alike.
		lines, err := sandbox.EnvPreludeLines(req.Env)
		if err != nil {
			return nil, err
		}
		stdin = lines + stdin
		args = append(args, "sh", "-c", sandbox.EnvPreludeScript(len(req.Env)), "sh")
	}
	args = append(args, req.Argv...)

	start := time.Now()
	stdout, stderr, truncated, exit, err := s.p.run.Run(ctx, strings.NewReader(stdin), nil, s.p.bin, args...)
	if err != nil {
		return nil, fmt.Errorf("exec in sandbox %s: %w", s.ID(), err)
	}
	return &sandbox.ExecResult{
		ExitCode:  exit,
		Stdout:    stdout,
		Stderr:    stderr,
		Truncated: truncated,
		Duration:  time.Since(start),
	}, nil
}

// putFileAttempts bounds how often a short copy is re-sent before the
// transfer is refused. One retry would cover a single unlucky stream; three
// attempts cover a cluster that drops one regularly without hiding one that
// drops every time, which the final error then names.
const putFileAttempts = 3

// PutFile streams a host file into the sandbox pod and applies mode (octal
// string, default "0600") — adapter protocol §4.2. The copy pipes the file
// through `sh -c 'cat > "$1"'` with the destination as a positional
// parameter: no tar dependency in the image (kubectl cp needs one) and no
// shell interpolation of the path. Path allow-listing is the core's
// responsibility; the provider only moves bytes.
//
// The pod counts what landed and the count is compared with the host file's
// size, because `kubectl exec -i` can deliver a prefix: the exec stream is
// torn down when local stdin reaches EOF, before the remote `cat` has
// drained what is still in flight, and the copy then ends at a buffer
// boundary with a zero exit code. Reporting the host's own stat as
// BytesCopied — which this provider did until issue #272 — turns that into
// a silent success, and the adapter goes on to judge a truncated artifact:
// the drill records source_corrupt against a backup that is intact, with a
// verdict that changes from run to run. A short copy is the transport's
// failure, not the backup's, so it is retried and then refused as a
// provider error, which reaches the adapter as sandbox_error (§4.3) and
// never as a statement about the backup.
//
// Size is the comparison rather than a digest because the image is the
// user's: `wc` is POSIX and present wherever the `sh` and `chmod` this
// function already needs are, while no checksum tool is guaranteed. It
// catches the truncation that actually happens here; a pod that answers
// with no count at all fails loudly rather than being taken on trust.
func (s *Sandbox) PutFile(ctx context.Context, hostPath, destPath, mode string) (*sandbox.PutFileResult, error) {
	if mode == "" {
		mode = "0600"
	}
	if _, err := strconv.ParseUint(mode, 8, 32); err != nil {
		return nil, fmt.Errorf("%w: mode %q is not octal", sandbox.ErrInvalidParams, mode)
	}
	info, err := os.Stat(hostPath)
	if err != nil {
		return nil, fmt.Errorf("put_file source: %w", err)
	}

	start := time.Now()
	landed, err := s.stream(ctx, hostPath, destPath, info.Size())
	if err != nil {
		return nil, err
	}
	if _, stderr, _, exit, err := s.p.run.Run(ctx, nil, nil, s.p.bin,
		"exec", "-n", s.namespace, s.pod, "--", "chmod", mode, destPath); err != nil {
		return nil, fmt.Errorf("chmod in sandbox %s: %w", s.ID(), err)
	} else if exit != 0 {
		return nil, fmt.Errorf("chmod in sandbox %s: exited %d: %s", s.ID(), exit, firstLine(stderr))
	}
	return &sandbox.PutFileResult{BytesCopied: landed, Duration: time.Since(start)}, nil
}

// stream sends the file and returns the byte count the pod reported, having
// established that it equals want. The file is reopened per attempt because
// a retry needs the reader from the beginning.
func (s *Sandbox) stream(ctx context.Context, hostPath, destPath string, want int64) (int64, error) {
	var last int64
	for attempt := 1; attempt <= putFileAttempts; attempt++ {
		landed, err := s.streamOnce(ctx, hostPath, destPath)
		if err != nil {
			return 0, err
		}
		if landed == want {
			return landed, nil
		}
		last = landed
		s.p.logger.Warn("short copy into sandbox, retrying",
			"id", s.ID(), "dest", destPath, "want_bytes", want,
			"landed_bytes", landed, "attempt", attempt, "attempts", putFileAttempts)
	}
	return 0, fmt.Errorf("copy into sandbox %s: %s received %d of %d bytes on each of %d attempts — the pod is not draining the exec stream; the artifact is not what was sent",
		s.ID(), destPath, last, want, putFileAttempts)
}

func (s *Sandbox) streamOnce(ctx context.Context, hostPath, destPath string) (int64, error) {
	f, err := os.Open(hostPath)
	if err != nil {
		return 0, fmt.Errorf("put_file source: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only descriptor
	stdout, stderr, _, exit, err := s.p.run.Run(ctx, f, nil, s.p.bin,
		"exec", "-n", s.namespace, "-i", s.pod, "--",
		"sh", "-c", `cat > "$1" && wc -c < "$1"`, "sh", destPath)
	if err != nil {
		return 0, fmt.Errorf("copy into sandbox %s: %w", s.ID(), err)
	}
	if exit != 0 {
		return 0, fmt.Errorf("copy into sandbox %s: exited %d: %s", s.ID(), exit, firstLine(stderr))
	}
	landed, err := strconv.ParseInt(strings.TrimSpace(string(stdout)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("copy into sandbox %s: the pod reported no byte count (%q) — the sandbox image needs a POSIX wc for the transfer to be verifiable",
			s.ID(), firstLine(stdout))
	}
	return landed, nil
}

// Destroy deletes the Job and, through foreground cascading, its pod — the
// call returns only when both are gone. It is idempotent: destroying an
// already-removed sandbox succeeds.
func (s *Sandbox) Destroy(ctx context.Context) error {
	if err := s.p.remove(ctx, s.namespace, s.job); err != nil {
		return fmt.Errorf("destroy sandbox: %w", err)
	}
	s.p.logger.Info("sandbox destroyed", "id", s.ID())
	return nil
}

func (p *Provider) remove(ctx context.Context, namespace, job string) error {
	_, stderr, _, exit, err := p.run.Run(ctx, nil, nil, p.bin,
		"delete", "job", "-n", namespace, job,
		"--cascade=foreground", "--ignore-not-found")
	if err != nil {
		return err
	}
	if exit != 0 {
		return fmt.Errorf("kubectl delete exited %d: %s", exit, firstLine(stderr))
	}
	return nil
}

// manifest builds the Job object from drill-config sandbox params. The
// accepted parameters are exactly what the descriptor declares — anything
// else is an error, because a typo must not silently weaken a sandbox —
// and a declared parameter this function does not implement is an error
// too. The descriptor is a parameter so tests can drive both failure paths.
func (p *Provider) manifest(d sandbox.Descriptor, params map[string]string) (*jobManifest, string, error) {
	image := params["image"]
	if image == "" {
		return nil, "", fmt.Errorf(`%w: "image" is required for the k8s provider`, sandbox.ErrInvalidParams)
	}
	namespace := "default"
	if ns, ok := params["namespace"]; ok && ns != "" {
		namespace = ns
	}
	limits := map[string]string{}
	var env []envVar
	for _, k := range sortedKeys(params) {
		v := params[k]
		spec, ok := d.Lookup(k)
		if !ok {
			return nil, "", d.UnknownParamError(k)
		}
		switch spec.Name {
		case "image", "namespace", "command":
			// Consumed above, or set on the container below.
		case "memory":
			limits["memory"] = v
		case "cpus":
			limits["cpu"] = v
		case "env.":
			name := strings.TrimPrefix(k, "env.")
			if !sandbox.ValidEnvName(name) {
				return nil, "", fmt.Errorf("%w: %q is not a valid environment variable name", sandbox.ErrInvalidParams, name)
			}
			env = append(env, envVar{Name: name, Value: v})
		default:
			return nil, "", d.UnhandledParamError(k)
		}
	}
	var resources *resourceSpec
	if len(limits) > 0 {
		// requests == limits: guaranteed resources, no surprise eviction of
		// a pod holding restored production data.
		resources = &resourceSpec{Limits: limits, Requests: limits}
	}
	labels := map[string]string{
		LabelSandbox: "1",
		labelPID:     sandbox.OwnerID(p.pid),
		labelHost:    p.hostID,
	}
	m := &jobManifest{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Metadata:   metadata{Name: "probavi-sbx-" + randomSuffix(), Labels: labels},
		Spec: jobSpec{
			BackoffLimit:            0, // a failed pod is a failed drill, never a silent retry
			ActiveDeadlineSeconds:   activeDeadlineSeconds,
			TTLSecondsAfterFinished: ttlSecondsAfterFinished,
			Template: podTemplate{
				Metadata: metadata{Labels: labels},
				Spec: podSpec{
					RestartPolicy:                "Never",
					AutomountServiceAccountToken: false, // a pod full of production data holds no cluster credentials
					EnableServiceLinks:           false,
					SecurityContext:              securityContext{SeccompProfile: seccompProfile{Type: "RuntimeDefault"}},
					Containers: []container{{
						Name:      "sandbox",
						Image:     image,
						Command:   strings.Fields(params["command"]),
						Env:       env,
						Resources: resources,
					}},
				},
			},
		},
	}
	return m, namespace, nil
}

// awaitRunning polls until the Job's pod exists and reports phase Running,
// then records the pod name for exec/put_file. A pod that already finished
// or failed is an error: the sandbox must be alive when provision starts.
func (p *Provider) awaitRunning(ctx context.Context, sbx *Sandbox) error {
	ctx, cancel := context.WithTimeout(ctx, p.awaitCap)
	defer cancel()
	lastState := "no pod yet"
	for {
		name, phase, reason, err := p.podStatus(ctx, sbx)
		if err != nil {
			return err
		}
		switch phase {
		case "Running":
			sbx.pod = name
			return nil
		case "Failed", "Succeeded":
			return fmt.Errorf("sandbox %s pod %s ended with phase %s before the drill started", sbx.ID(), name, phase)
		case "":
		default:
			lastState = phase
			if reason != "" {
				lastState = phase + " (" + reason + ")"
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("sandbox %s never reached running state (last: %s): %w", sbx.ID(), lastState, ctx.Err())
		case <-time.After(p.awaitInterval):
		}
	}
}

// podStatus returns the Job's single pod name, phase, and — while waiting —
// the container's waiting reason (e.g. ImagePullBackOff) for diagnostics.
func (p *Provider) podStatus(ctx context.Context, sbx *Sandbox) (name, phase, reason string, err error) {
	stdout, stderr, _, exit, err := p.run.Run(ctx, nil, nil, p.bin,
		"get", "pods", "-n", sbx.namespace, "-l", "job-name="+sbx.job, "-o", "json")
	if err != nil {
		return "", "", "", fmt.Errorf("await sandbox %s: %w", sbx.ID(), err)
	}
	if exit != 0 {
		return "", "", "", fmt.Errorf("await sandbox %s: kubectl get pods exited %d: %s", sbx.ID(), exit, firstLine(stderr))
	}
	var list podList
	if err := json.Unmarshal(stdout, &list); err != nil {
		return "", "", "", fmt.Errorf("await sandbox %s: parse pod list: %w", sbx.ID(), err)
	}
	if len(list.Items) == 0 {
		return "", "", "", nil
	}
	pod := list.Items[0]
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting != nil {
			reason = cs.State.Waiting.Reason
		}
	}
	return pod.Metadata.Name, pod.Status.Phase, reason, nil
}

// Kubernetes object shapes — only the fields this provider reads or writes.

type jobManifest struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   metadata `json:"metadata"`
	Spec       jobSpec  `json:"spec"`
}

type metadata struct {
	Name   string            `json:"name,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
}

type jobSpec struct {
	BackoffLimit            int         `json:"backoffLimit"`
	ActiveDeadlineSeconds   int         `json:"activeDeadlineSeconds"`
	TTLSecondsAfterFinished int         `json:"ttlSecondsAfterFinished"`
	Template                podTemplate `json:"template"`
}

type podTemplate struct {
	Metadata metadata `json:"metadata"`
	Spec     podSpec  `json:"spec"`
}

type podSpec struct {
	RestartPolicy                string          `json:"restartPolicy"`
	AutomountServiceAccountToken bool            `json:"automountServiceAccountToken"`
	EnableServiceLinks           bool            `json:"enableServiceLinks"`
	SecurityContext              securityContext `json:"securityContext"`
	Containers                   []container     `json:"containers"`
}

type securityContext struct {
	SeccompProfile seccompProfile `json:"seccompProfile"`
}

type seccompProfile struct {
	Type string `json:"type"`
}

type container struct {
	Name      string        `json:"name"`
	Image     string        `json:"image"`
	Command   []string      `json:"command,omitempty"`
	Env       []envVar      `json:"env,omitempty"`
	Resources *resourceSpec `json:"resources,omitempty"`
}

type envVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type resourceSpec struct {
	Limits   map[string]string `json:"limits,omitempty"`
	Requests map[string]string `json:"requests,omitempty"`
}

type jobList struct {
	Items []struct {
		Metadata struct {
			Name      string            `json:"name"`
			Namespace string            `json:"namespace"`
			Labels    map[string]string `json:"labels"`
		} `json:"metadata"`
	} `json:"items"`
}

type podList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Status struct {
			Phase             string `json:"phase"`
			ContainerStatuses []struct {
				State struct {
					Waiting *struct {
						Reason string `json:"reason"`
					} `json:"waiting"`
				} `json:"state"`
			} `json:"containerStatuses"`
		} `json:"status"`
	} `json:"items"`
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

// factsPath asks the API server for what the pod actually runs and what
// the kubelet recorded as its container's limits. imageID is the digest
// the node resolved the tag to, which is the fact a record wants — `image`
// is the tag, and a tag points at different bytes over time.
const factsPath = `{.status.containerStatuses[0].imageID} ` +
	`{.spec.containers[0].resources.limits.memory} ` +
	`{.spec.containers[0].resources.limits.cpu}`

// Facts reports what this pod actually ran and the limits its spec carries
// (sandbox-providers.md §6.1).
//
// A Job without limits is the ordinary case here and reports nil for both:
// a pod with no limits takes what the node has, which is not a number this
// record may state. That is the same absence docker's zero produces, and
// it is why §6.1 makes null always acceptable.
func (s *Sandbox) Facts(ctx context.Context) sandbox.Facts {
	stdout, stderr, _, exit, err := s.p.run.Run(ctx, nil, nil, s.p.bin,
		"get", "pod", s.pod, "-n", s.namespace, "-o", "jsonpath="+factsPath)
	if err != nil || exit != 0 {
		s.p.logger.Debug("sandbox facts unavailable", "pod", s.pod, "exit", exit,
			"err", err, "stderr", firstLine(stderr))
		return sandbox.Facts{}
	}
	fields := strings.Fields(string(stdout))
	facts := sandbox.Facts{}
	if len(fields) > 0 {
		facts.ImageDigest = imageDigestOrNil(fields[0])
	}
	if len(fields) > 1 {
		facts.MemoryBytes = quantityBytes(fields[1])
	}
	if len(fields) > 2 {
		facts.CPUsMilli = quantityMilli(fields[2])
	}
	return facts
}

// imageDigestOrNil extracts the digest from a container status's imageID,
// which the kubelet writes as "<repo>@sha256:<hex>" for an image it pulled
// by digest and may write without one for an image loaded locally. Only
// the published form is recorded; anything else records nothing.
func imageDigestOrNil(imageID string) *string {
	_, digest, found := strings.Cut(imageID, "@")
	if !found || !imageDigestPattern.MatchString(digest) {
		return nil
	}
	return &digest
}

var imageDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// quantityBytes reads a Kubernetes memory quantity — a plain number of
// bytes, or one with a binary or decimal suffix. Anything it cannot read
// is nil rather than a guess.
func quantityBytes(q string) *int64 {
	multipliers := []struct {
		suffix string
		factor int64
	}{
		{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40},
		{"k", 1000}, {"M", 1000 * 1000}, {"G", 1000 * 1000 * 1000}, {"T", 1e12},
	}
	for _, m := range multipliers {
		if num, ok := strings.CutSuffix(q, m.suffix); ok {
			return positiveOrNil(num, m.factor)
		}
	}
	return positiveOrNil(q, 1)
}

// quantityMilli reads a Kubernetes CPU quantity, which is either a number
// of CPUs or a millicore count written with an "m" suffix.
func quantityMilli(q string) *int64 {
	if num, ok := strings.CutSuffix(q, "m"); ok {
		return positiveOrNil(num, 1)
	}
	return positiveOrNil(q, 1000)
}

// positiveOrNil multiplies by factor and returns nil for anything that is
// not a positive number, so "no limit" stays an absence rather than
// becoming a limit of zero. Fractional CPU counts are accepted because a
// spec may carry 1.5; the schema's integer rule is satisfied by the
// thousandths the caller asks for.
func positiveOrNil(s string, factor int64) *int64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || f <= 0 {
		return nil
	}
	v := int64(f * float64(factor))
	if v <= 0 {
		return nil
	}
	return &v
}
