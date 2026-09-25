// Package adapter is the core-side client of the Probavi adapter protocol
// (docs/adapter-protocol.md, normative). It launches adapter executables,
// speaks line-delimited JSON with them, mediates sandbox verbs, and
// enforces every framing rule of the spec: anything an adapter does outside
// the protocol is an adapter_crash, never undefined behavior.
package adapter

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"slices"
	"time"
)

// ProtocolVersion is the highest protocol version this client speaks, and
// ProtocolFloor the lowest. Both are published: an adapter declaring only
// the floor is driven unchanged, forever (protocol §8).
const (
	ProtocolVersion = "probavi-adapter/1"
	ProtocolFloor   = "probavi-adapter/0"
)

// protocolVersions are the versions this client speaks, highest first.
// Negotiation walks it in order and takes the first the adapter also
// declares, which is §8's "highest common one".
var protocolVersions = []string{ProtocolVersion, ProtocolFloor}

// ProtocolVersions returns the versions this client speaks, highest first.
func ProtocolVersions() []string { return slices.Clone(protocolVersions) }

// maxLineBytes is the protocol's frame size limit (§2.2).
const maxLineBytes = 4 << 20

// defaultGrace is the SIGTERM→SIGKILL grace period (§2.4).
const defaultGrace = 10 * time.Second

// SandboxPasswordEnv names the ephemeral per-drill secret the core
// generates and passes to the adapter (§2.5). An adapter that sets an
// authenticated superuser password to this value references it back
// through connection.password_env. The constant holds the variable's
// NAME, never a secret: §2.5 exists so that values travel in the
// environment while names travel in the protocol.
//
//nolint:gosec // G101 matches the identifier here, not a credential.
const SandboxPasswordEnv = "PROBAVI_SANDBOX_PASSWORD"

// Error codes from the protocol registry (§5) that this client emits or
// callers commonly match on.
const (
	CodeInvalidRequest = "invalid_request"
	CodeSandboxError   = "sandbox_error"
	CodeAdapterCrash   = "adapter_crash"
	CodeCancelled      = "cancelled"
)

// Error is a protocol error object (§5): either sent by the adapter in a
// final response, or assigned by this client (adapter_crash).
type Error struct {
	Code      string          `json:"code"`
	Message   string          `json:"message"`
	Retryable bool            `json:"retryable"`
	Detail    json.RawMessage `json:"detail,omitempty"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("adapter error %s: %s", e.Code, e.Message)
}

func crashf(format string, a ...any) *Error {
	return &Error{Code: CodeAdapterCrash, Message: fmt.Sprintf(format, a...), Retryable: false}
}

// Options tune a Runner. The zero value is valid.
type Options struct {
	// CredentialEnv names variables passed through from the core's own
	// environment (drill config source.credential_env).
	CredentialEnv []string
	// Env sets explicit extra variables (e.g. SandboxPasswordEnv).
	// Values never appear in protocol messages or logs.
	Env map[string]string
	// Grace is the SIGTERM→SIGKILL period; default 10s.
	Grace time.Duration
}

// Runner launches one adapter executable, one fresh process per operation.
type Runner struct {
	path   string
	logger *slog.Logger
	opts   Options

	// negotiated is the version chosen from the probe response and used
	// for every later operation of this drill. Empty until Probe has
	// answered, which is why the probe itself goes out at the floor: the
	// message that discovers the version cannot be sent at a version the
	// adapter might refuse (§6.1).
	negotiated string
}

// Protocol is the version this runner is speaking. Before a probe has
// answered it is the floor, which is what the probe request carries and
// therefore what a record should say about a drill that got no further.
func (r *Runner) Protocol() string {
	if r.negotiated == "" {
		return ProtocolFloor
	}
	return r.negotiated
}

// Path is the executable this runner launches, as resolved from the
// adapter name (§2.1).
//
// The core needs it to record which build produced a drill's evidence
// (evidence-schema.md §3), but the hashing happens there, not here: the
// "sha256:" form is an evidence-schema convention, and this package
// deliberately knows nothing about the record format — the same boundary
// TestEvidenceTimestampFormatMatches guards.
func (r *Runner) Path() string { return r.path }

// New resolves the adapter name to the executable probavi-adapter-<name> on
// PATH (§2.1) and returns a Runner for it.
func New(name string, logger *slog.Logger, opts *Options) (*Runner, error) {
	path, err := exec.LookPath("probavi-adapter-" + name)
	if err != nil {
		return nil, fmt.Errorf("resolve adapter %q: %w", name, err)
	}
	return newRunner(path, logger, opts), nil
}

func newRunner(path string, logger *slog.Logger, opts *Options) *Runner {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	o := Options{}
	if opts != nil {
		o = *opts
	}
	if o.Grace <= 0 {
		o.Grace = defaultGrace
	}
	return &Runner{path: path, logger: logger, opts: o}
}

func newRequestID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// The id only needs uniqueness within one adapter process; a
		// failing crypto/rand is unrecoverable anyway, panic loudly.
		panic("adapter: crypto/rand unavailable: " + err.Error())
	}
	return "r-" + hex.EncodeToString(b[:])
}
