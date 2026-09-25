package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

// evidenceTimestampFormat is the evidence schema's timestamp form (§3):
// RFC 3339 UTC with exactly millisecond precision. It is duplicated here
// rather than imported so that the protocol client keeps knowing nothing
// about the evidence package; TestEvidenceTimestampFormatMatches pins the
// two constants together, so the copy cannot drift.
const evidenceTimestampFormat = "2006-01-02T15:04:05.000Z"

// Wire envelope (§3). Exactly one of SandboxCall (adapter→core) or OK
// (final response) is present in adapter output.
type envelope struct {
	Protocol    string          `json:"protocol"`
	RequestID   string          `json:"request_id"`
	Op          string          `json:"op,omitempty"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	SandboxCall *sandboxCall    `json:"sandbox_call,omitempty"`
	OK          *bool           `json:"ok,omitempty"`
	Error       *Error          `json:"error,omitempty"`
}

type sandboxCall struct {
	CallID string          `json:"call_id"`
	Verb   string          `json:"verb"`
	Args   json.RawMessage `json:"args"`
}

type sandboxResult struct {
	CallID string `json:"call_id"`
	OK     bool   `json:"ok"`
	Value  any    `json:"value,omitempty"`
	Error  *Error `json:"error,omitempty"`
}

type sandboxResultEnvelope struct {
	Protocol      string        `json:"protocol"`
	RequestID     string        `json:"request_id"`
	SandboxResult sandboxResult `json:"sandbox_result"`
}

// ProbeResult is the probe response payload (§6.1).
type ProbeResult struct {
	Name             string       `json:"name"`
	AdapterVersion   string       `json:"adapter_version"`
	ProtocolVersions []string     `json:"protocol_versions"`
	Engine           Engine       `json:"engine"`
	Sources          []SourceKind `json:"sources"`
	SQLRunner        SQLRunner    `json:"sql_runner"`
	VerbsRequired    []string     `json:"verbs_required"`

	// Identifier and Checks are v1's (§6.1.1), both optional. Absent —
	// which is every v0 adapter and any v1 adapter with nothing to
	// declare — means the core composes its own statements and quotes
	// SQL-standard, exactly as it always has.
	Identifier *Identifier               `json:"identifier,omitempty"`
	Checks     map[string]CheckStatement `json:"checks,omitempty"`
}

// Identifier is how an engine spells a qualified name (§6.1.1). Open and
// Close are the quoting characters — both empty for an engine that takes
// bare names — and Separator joins the parts of a qualified one.
type Identifier struct {
	Open      string `json:"open"`
	Close     string `json:"close"`
	Separator string `json:"separator"`
}

// CheckStatement is what an adapter declares for one built-in check kind.
// Statement is not required to be SQL: it is whatever this engine's
// sql_runner takes, which for several engines is not SQL at all.
type CheckStatement struct {
	Statement string `json:"statement"`
}

// Engine identifies the database engine an adapter drives.
type Engine struct {
	Name string `json:"name"`
}

// SourceKind is one supported backup source kind with its capabilities.
type SourceKind struct {
	Kind         string       `json:"kind"`
	Capabilities Capabilities `json:"capabilities"`
}

// Capabilities flags optional adapter features per source kind.
//
// Select is a pointer because absent and false are different answers
// (§6.1.2): absent means the kind has not been asked — every v0 adapter —
// while false is a declaration the core acts on by refusing a selection
// policy before a sandbox exists.
type Capabilities struct {
	PITR   bool  `json:"pitr"`
	Select *bool `json:"select,omitempty"`
}

// SQLRunner is the declarative check-execution template (§6.1) that lets
// the core run SQL checks without learning engine concepts.
type SQLRunner struct {
	Argv []string          `json:"argv"`
	Env  map[string]string `json:"env"`
}

// ProvisionRequest is the provision request payload (§6.2).
type ProvisionRequest struct {
	Source  ProvisionSource   `json:"source"`
	Sandbox SandboxInfo       `json:"sandbox"`
	Options map[string]string `json:"options"`
	PITR    *PITR             `json:"pitr,omitempty"`
}

// ProvisionSource describes the backup source handed to the adapter.
type ProvisionSource struct {
	Kind          string            `json:"kind"`
	Path          string            `json:"path"`
	Params        map[string]string `json:"params"`
	CredentialEnv []string          `json:"credential_env"`
}

// SandboxInfo carries the provider guarantees the adapter may rely on.
type SandboxInfo struct {
	ScratchDir string `json:"scratch_dir"`
}

// PITR requests point-in-time recovery (only sent when probe declared the
// capability).
type PITR struct {
	TargetTime string `json:"target_time"`
}

// ProvisionResult is the provision response payload (§6.2).
type ProvisionResult struct {
	Connection     Connection      `json:"connection"`
	SourceIdentity SourceIdentity  `json:"source_identity"`
	Timings        Timings         `json:"timings"`
	State          json.RawMessage `json:"state"`
}

// Connection describes reachability from inside the sandbox. PasswordEnv
// names an environment variable — never a value (§2.5).
type Connection struct {
	Scheme      string `json:"scheme"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Database    string `json:"database"`
	User        string `json:"user"`
	PasswordEnv string `json:"password_env,omitempty"`
}

// SourceIdentity feeds the evidence record's backup identity.
type SourceIdentity struct {
	Checksum  string  `json:"checksum"`
	SizeBytes int64   `json:"size_bytes"`
	CreatedAt *string `json:"created_at"`
}

// Timings are the adapter-measured phases in seconds (§7); the core
// converts to integer milliseconds for evidence.
type Timings struct {
	EngineReadySeconds float64 `json:"engine_ready_seconds"`
	TransferSeconds    float64 `json:"transfer_seconds"`
	RestoreSeconds     float64 `json:"restore_seconds"`
}

// HealthcheckResult is the healthcheck response payload (§6.3).
type HealthcheckResult struct {
	Healthy        bool    `json:"healthy"`
	LatencySeconds float64 `json:"latency_seconds"`
	Detail         string  `json:"detail"`
}

// TeardownResult is the teardown response payload (§6.4).
type TeardownResult struct {
	Released bool `json:"released"`
}

var checksumPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Probe runs the probe operation. Sandbox calls are forbidden during probe.
func (r *Runner) Probe(ctx context.Context) (*ProbeResult, error) {
	payload, err := r.do(ctx, "probe", struct{}{}, nil, nil)
	if err != nil {
		return nil, err
	}
	res := &ProbeResult{}
	if err := json.Unmarshal(payload, res); err != nil {
		return nil, crashf("probe payload: %v", err)
	}
	if res.Name == "" {
		return nil, crashf("probe payload: name is empty")
	}
	chosen := highestCommon(res.ProtocolVersions)
	if chosen == "" {
		return nil, &Error{Code: "unsupported_protocol",
			Message: fmt.Sprintf("adapter speaks %v, core speaks %v", res.ProtocolVersions, protocolVersions)}
	}
	r.negotiated = chosen
	return res, nil
}

// Provision runs the provision operation, mediating sandbox verbs; put_file
// sources are restricted to the drill's configured backup source path.
func (r *Runner) Provision(ctx context.Context, req *ProvisionRequest, verbs SandboxVerbs) (*ProvisionResult, error) {
	if verbs == nil {
		return nil, fmt.Errorf("provision: sandbox verbs are required")
	}
	normalizeProvisionRequest(req)
	payload, err := r.do(ctx, "provision", req, verbs, sourceGuard(req.Source.Path))
	if err != nil {
		return nil, err
	}
	res := &ProvisionResult{}
	if err := json.Unmarshal(payload, res); err != nil {
		return nil, crashf("provision payload: %v", err)
	}
	return res, validateProvisionResult(res)
}

// Healthcheck runs the healthcheck operation.
func (r *Runner) Healthcheck(ctx context.Context, conn *Connection, state json.RawMessage, verbs SandboxVerbs) (*HealthcheckResult, error) {
	req := struct {
		Connection *Connection     `json:"connection"`
		State      json.RawMessage `json:"state"`
	}{conn, normalizeState(state)}
	payload, err := r.do(ctx, "healthcheck", req, verbs, nil)
	if err != nil {
		return nil, err
	}
	res := &HealthcheckResult{}
	if err := json.Unmarshal(payload, res); err != nil {
		return nil, crashf("healthcheck payload: %v", err)
	}
	return res, nil
}

// Teardown runs the teardown operation. It must be invoked after every
// provision outcome, including crashes; state may be empty (§6.4).
func (r *Runner) Teardown(ctx context.Context, state json.RawMessage, reason string, verbs SandboxVerbs) (*TeardownResult, error) {
	switch reason {
	case "completed", "failed", "timeout", "cancelled":
	default:
		return nil, fmt.Errorf("teardown: invalid reason %q", reason)
	}
	req := struct {
		State  json.RawMessage `json:"state"`
		Reason string          `json:"reason"`
	}{normalizeState(state), reason}
	payload, err := r.do(ctx, "teardown", req, verbs, nil)
	if err != nil {
		return nil, err
	}
	res := &TeardownResult{}
	if err := json.Unmarshal(payload, res); err != nil {
		return nil, crashf("teardown payload: %v", err)
	}
	return res, nil
}

func normalizeProvisionRequest(req *ProvisionRequest) {
	if req.Source.Params == nil {
		req.Source.Params = map[string]string{}
	}
	if req.Source.CredentialEnv == nil {
		req.Source.CredentialEnv = []string{}
	}
	if req.Options == nil {
		req.Options = map[string]string{}
	}
	if req.Sandbox.ScratchDir == "" {
		req.Sandbox.ScratchDir = "/tmp"
	}
}

// validateProvisionResult holds the response to everything the evidence
// record will demand of it, and normalizes what the record cannot take
// verbatim. This boundary exists so that a malformed value is an
// adapter_crash verdict with a signed record, never a drill that restored
// a backup and then failed to write its proof: the core composes records
// from these fields, and evidence.Record.Validate rejects — it does not
// repair.
func validateProvisionResult(res *ProvisionResult) error {
	if !checksumPattern.MatchString(res.SourceIdentity.Checksum) {
		return crashf("provision payload: source_identity.checksum %q is not a sha256 reference", res.SourceIdentity.Checksum)
	}
	if res.SourceIdentity.SizeBytes < 0 {
		return crashf("provision payload: source_identity.size_bytes is negative (%d)", res.SourceIdentity.SizeBytes)
	}
	created, err := normalizeCreatedAt(res.SourceIdentity.CreatedAt)
	if err != nil {
		return err
	}
	res.SourceIdentity.CreatedAt = created
	for _, t := range []struct {
		field   string
		seconds float64
	}{
		{"engine_ready_seconds", res.Timings.EngineReadySeconds},
		{"transfer_seconds", res.Timings.TransferSeconds},
		{"restore_seconds", res.Timings.RestoreSeconds},
	} {
		if err := validateTimingSeconds(t.field, t.seconds); err != nil {
			return err
		}
	}
	if res.Connection.Scheme == "" {
		return crashf("provision payload: connection.scheme is empty")
	}
	return nil
}

// maxTimingSeconds bounds a reported phase duration so that its integer
// millisecond form stays inside the evidence schema's safe-integer range
// (|n| <= 2^53-1). It is ~285,000 years: the point is to exclude nonsense
// a canonicalizer would refuse, not to judge how long a restore may take.
const maxTimingSeconds = float64((1<<53 - 1) / 1000)

// validateTimingSeconds rejects durations the record cannot represent.
// NaN needs its own test: every comparison against NaN is false, so a
// plain "< 0" guard passes it straight through to the record, where
// int64(math.Round(NaN)) becomes the most negative int64 there is.
func validateTimingSeconds(field string, seconds float64) error {
	switch {
	case math.IsNaN(seconds):
		return crashf("provision payload: timings.%s is NaN", field)
	case math.IsInf(seconds, 0):
		return crashf("provision payload: timings.%s is infinite", field)
	case seconds < 0:
		return crashf("provision payload: negative timings (%s = %g)", field, seconds)
	case seconds > maxTimingSeconds:
		return crashf("provision payload: timings.%s (%g) exceeds what an evidence record can represent", field, seconds)
	}
	return nil
}

// normalizeCreatedAt converts the adapter's RFC 3339 instant — any
// precision, any offset — into the evidence schema's UTC millisecond form
// (§6.2). Sub-millisecond digits are truncated, never rounded: rounding up
// would record a backup as newer than it is, and this value ends up in a
// signed document an auditor may read. A nil value stays nil: "not
// derivable" is a legitimate answer.
func normalizeCreatedAt(created *string) (*string, error) {
	if created == nil {
		return nil, nil
	}
	ts, err := parseRFC3339(*created)
	if err != nil {
		return nil, crashf("provision payload: source_identity.created_at %q is not an RFC 3339 instant", *created)
	}
	normalized := ts.UTC().Truncate(time.Millisecond).Format(evidenceTimestampFormat)
	return &normalized, nil
}

// parseRFC3339 accepts what RFC 3339 accepts, which is slightly more than
// Go's time.RFC3339 layout: the standard allows lowercase "t" and "z"
// designators, and docs/schemas/adapter/provision-response.json declares
// them valid ([Tt], [Zz]). An adapter whose output validates against the
// published schema must not be rejected here. RFC 3339 has no other
// letters, so upper-casing is a safe way to reach the strict layout.
func parseRFC3339(s string) (time.Time, error) {
	ts, err := time.Parse(time.RFC3339, s)
	if err == nil {
		return ts, nil
	}
	return time.Parse(time.RFC3339, strings.ToUpper(s))
}

func normalizeState(state json.RawMessage) json.RawMessage {
	if len(state) == 0 {
		return json.RawMessage("{}")
	}
	return state
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// highestCommon returns the highest version both sides speak, or "" when
// they share none. The core's list is ordered highest first, so the first
// match is the answer (§8).
func highestCommon(theirs []string) string {
	for _, v := range protocolVersions {
		if contains(theirs, v) {
			return v
		}
	}
	return ""
}
