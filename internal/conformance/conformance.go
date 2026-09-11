// Package conformance implements the frozen check list of
// docs/adapter-protocol.md §10: it drives an adapter executable exactly as
// the core would — one fresh process per operation, a simulated sandbox —
// and reports a pass/fail verdict per check. It is deliberately independent
// of internal/adapter: a second implementation of the protocol that
// validates adapters from the outside, with full visibility of every frame.
package conformance

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Check is one §10 check's verdict.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// Report is the machine-readable suite result. Passed+Failed always equals
// the frozen list's length: every check reports, even when an earlier
// failure made it unable to run meaningfully.
type Report struct {
	Adapter string  `json:"adapter"`
	Passed  int     `json:"passed"`
	Failed  int     `json:"failed"`
	Checks  []Check `json:"checks"`
}

func (r *Report) add(name string, ok bool, detail string) {
	r.Checks = append(r.Checks, Check{Name: name, OK: ok, Detail: detail})
	if ok {
		r.Passed++
	} else {
		r.Failed++
	}
}

// Options tune a suite run. The zero value is valid.
type Options struct {
	// SourceKind selects the kind for checks 8–10; default: the first kind
	// the probe declares (§10 — pick a logical kind).
	SourceKind string
	// SourceParams pass through as source.params for checks 8–10.
	SourceParams map[string]string
	// SourcePath is the artifact checks 9–10 provision from. Default: a
	// temporary file the suite generates, which suits an adapter whose
	// artifact is one file and cannot suit one whose artifact is a
	// directory — QuestDB's is the server's whole data root, and refusing
	// a file for it is correct behaviour rather than a conformance
	// failure. A path given here is used as it stands and never removed.
	SourcePath string
	// Grace is the §2.4 SIGTERM→SIGKILL period for check 14; default 10s.
	Grace time.Duration
}

var (
	adapterNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	checksumPattern    = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// probeResult is the §6.1 payload as the suite reads it.
type probeResult struct {
	Name             string   `json:"name"`
	ProtocolVersions []string `json:"protocol_versions"`
	Sources          []struct {
		Kind string `json:"kind"`
	} `json:"sources"`
	SQLRunner struct {
		Argv []string          `json:"argv"`
		Env  map[string]string `json:"env"`
	} `json:"sql_runner"`
	VerbsRequired []string `json:"verbs_required"`
}

// provisionResult is the §6.2 payload as the suite reads it.
type provisionResult struct {
	Connection struct {
		Scheme string `json:"scheme"`
	} `json:"connection"`
	SourceIdentity struct {
		Checksum  string  `json:"checksum"`
		CreatedAt *string `json:"created_at"`
	} `json:"source_identity"`
	Timings struct {
		EngineReadySeconds float64 `json:"engine_ready_seconds"`
		TransferSeconds    float64 `json:"transfer_seconds"`
		RestoreSeconds     float64 `json:"restore_seconds"`
	} `json:"timings"`
	State json.RawMessage `json:"state"`
}

// suite carries one run's state across checks.
type suite struct {
	ctx     context.Context
	path    string
	opts    Options
	report  *Report
	framing []string // aggregated §2.2/§3 violations across every operation
	err     error    // sticky harness-side failure; makes later drives inert

	probe     *probeResult
	provision *provisionResult
	rawConn   json.RawMessage // provision connection, passed to healthcheck verbatim
}

// Run executes the frozen §10 check list against the adapter executable.
// The returned error reports harness-side failures only (temp files,
// unstartable process); adapter verdicts live in the Report.
func Run(ctx context.Context, adapterPath string, opts Options) (*Report, error) {
	if opts.Grace <= 0 {
		opts.Grace = 10 * time.Second
	}
	s := &suite{
		ctx:    ctx,
		path:   adapterPath,
		opts:   opts,
		report: &Report{Adapter: adapterPath, Checks: []Check{}},
	}
	steps := []func(){
		s.checkProbe, s.checkHandshake, s.checkProvisionErrors,
		s.checkProvisionHappyPath, s.checkHealthcheck, s.checkTeardown,
		s.checkSigterm,
	}
	for _, step := range steps {
		step()
		if s.err != nil {
			return nil, s.err
		}
	}
	s.checkFraming()
	return s.report, nil
}

// driveOp runs one operation and collects its framing observations for
// check 15. Harness-side failures (unstartable process, dead context) are
// not adapter verdicts: they park in s.err, later drives become inert, and
// Run reports the error instead of a report.
func (s *suite) driveOp(op string, payload any, spec driveSpec) *opResult {
	if s.err != nil {
		return &opResult{}
	}
	rid := requestID()
	if spec.request == "" {
		spec.request = request(rid, op, payload)
	}
	res, err := drive(s.ctx, s.path, rid, spec)
	if err != nil {
		s.err = fmt.Errorf("drive %s: %w", op, err)
		return &opResult{}
	}
	s.framing = append(s.framing, res.violations...)
	return res
}

// checkProbe covers checks 1–3.
func (s *suite) checkProbe() {
	res := s.driveOp("probe", map[string]any{}, driveSpec{})
	pr := &probeResult{}
	shape := s.probeShape(res, pr)
	s.report.add("probe.shape", shape == "", shape)
	if shape == "" {
		s.probe = pr
	}

	runner := sqlRunnerProblem(pr)
	s.report.add("probe.sql_runner", runner == "", runner)

	if len(res.calls) == 0 {
		s.report.add("probe.no_sandbox_calls", true, "")
	} else {
		s.report.add("probe.no_sandbox_calls", false,
			fmt.Sprintf("probe issued %d sandbox call(s); §6.1 forbids any", len(res.calls)))
	}
}

func (s *suite) probeShape(res *opResult, pr *probeResult) string {
	if !res.finalOK() {
		return "probe did not return an ok final response (exit " + fmt.Sprint(res.exitCode) + ")"
	}
	if err := json.Unmarshal(res.final.Payload, pr); err != nil {
		return "probe payload is not a §6.1 object: " + err.Error()
	}
	if !adapterNamePattern.MatchString(pr.Name) {
		return fmt.Sprintf("name %q must match %s", pr.Name, adapterNamePattern)
	}
	if !contains(pr.ProtocolVersions, protocolVersion) {
		return fmt.Sprintf("protocol_versions %v must contain %q", pr.ProtocolVersions, protocolVersion)
	}
	if len(pr.Sources) == 0 {
		return "at least one source kind is required"
	}
	for _, src := range pr.Sources {
		if src.Kind == "" {
			return "a source kind is empty"
		}
	}
	for _, verb := range pr.VerbsRequired {
		if verb != "exec" && verb != "put_file" {
			return fmt.Sprintf("verbs_required contains %q; v0 defines exec and put_file only", verb)
		}
	}
	return ""
}

func sqlRunnerProblem(pr *probeResult) string {
	if pr.Name == "" {
		return "unverifiable: probe.shape failed"
	}
	if !contains(pr.SQLRunner.Argv, "{{sql}}") {
		return "sql_runner.argv must carry {{sql}} as its own element (§6.1)"
	}
	for _, a := range pr.SQLRunner.Argv {
		if a != "{{sql}}" && a != "{{user}}" && a != "{{database}}" && containsPassword(a) {
			return "{{password}} may appear only in sql_runner.env values (§6.1)"
		}
	}
	return ""
}

func containsPassword(s string) bool {
	return regexp.MustCompile(`{{password}}`).MatchString(s)
}

// checkHandshake covers checks 4–5.
func (s *suite) checkHandshake() {
	rid := requestID()
	line := fmt.Sprintf(
		`{"protocol":"probavi-adapter/999","request_id":%q,"op":"probe","payload":{}}`, rid)
	res := s.driveRaw(rid, driveSpec{request: line})
	s.report.add("handshake.unsupported_protocol",
		res.finalError() == "unsupported_protocol" && supportedListed(res.final),
		detailUnless(res.finalError() == "unsupported_protocol" && supportedListed(res.final),
			fmt.Sprintf("got code %q, detail %s; want unsupported_protocol with detail.supported (§3.1)",
				res.finalError(), errorDetail(res.final))))

	res = s.driveOp("probavi-conformance-unknown-op", map[string]any{}, driveSpec{})
	s.report.add("handshake.unknown_op",
		res.finalError() == "invalid_request",
		detailUnless(res.finalError() == "invalid_request",
			fmt.Sprintf("got code %q, want invalid_request", res.finalError())))
}

// driveRaw is driveOp for checks that hand-craft the request line.
func (s *suite) driveRaw(rid string, spec driveSpec) *opResult {
	if s.err != nil {
		return &opResult{}
	}
	res, err := drive(s.ctx, s.path, rid, spec)
	if err != nil {
		s.err = fmt.Errorf("drive raw request: %w", err)
		return &opResult{}
	}
	s.framing = append(s.framing, res.violations...)
	return res
}

func supportedListed(final *envelope) bool {
	if final == nil || final.Error == nil || final.Error.Detail == nil {
		return false
	}
	var detail struct {
		Supported []string `json:"supported"`
	}
	if err := json.Unmarshal(final.Error.Detail, &detail); err != nil {
		return false
	}
	return len(detail.Supported) > 0
}

func errorDetail(final *envelope) string {
	if final == nil || final.Error == nil || final.Error.Detail == nil {
		return "absent"
	}
	return string(final.Error.Detail)
}

// checkProvisionErrors covers checks 6–8.
func (s *suite) checkProvisionErrors() {
	res := s.driveOp("provision", "probavi-conformance-garbage", driveSpec{})
	ok := res.finalError() == "invalid_request" && res.exitCode == 0
	s.report.add("provision.malformed_payload", ok,
		detailUnless(ok, fmt.Sprintf("got code %q exit %d, want invalid_request with exit 0",
			res.finalError(), res.exitCode)))

	res = s.driveOp("provision", s.provisionPayload("probavi-conformance-unsupported", "/nonexistent"), driveSpec{})
	ok = res.finalError() == "unsupported_source"
	s.report.add("provision.unsupported_source", ok,
		detailUnless(ok, fmt.Sprintf("got code %q, want unsupported_source (§5)", res.finalError())))

	missing := filepath.Join(os.TempDir(), "probavi-conformance-missing-"+requestID())
	res = s.driveOp("provision", s.provisionPayload(s.sourceKind(), missing), driveSpec{})
	ok = res.finalError() == "source_not_found"
	s.report.add("provision.missing_source", ok,
		detailUnless(ok, fmt.Sprintf("got code %q, want source_not_found (§5)", res.finalError())))
}

// checkProvisionHappyPath covers checks 9–10.
func (s *suite) checkProvisionHappyPath() {
	source, cleanup, err := s.source()
	if err != nil {
		s.err = err
		return
	}
	defer cleanup()

	res := s.driveOp("provision", s.provisionPayload(s.sourceKind(), source), driveSpec{})
	pr := &provisionResult{}
	problem := happyPathProblem(res, pr)
	s.report.add("provision.happy_path", problem == "", problem)
	if problem == "" {
		s.provision = pr
		var full struct {
			Connection json.RawMessage `json:"connection"`
		}
		if err := json.Unmarshal(res.final.Payload, &full); err == nil {
			s.rawConn = full.Connection
		}
	}

	timings := timingsProblem(res, pr, problem)
	s.report.add("provision.timings", timings == "", timings)
}

func happyPathProblem(res *opResult, pr *provisionResult) string {
	if !res.finalOK() {
		return fmt.Sprintf("provision failed with code %q against the simulated sandbox", res.finalError())
	}
	if err := json.Unmarshal(res.final.Payload, pr); err != nil {
		return "provision payload is not a §6.2 object: " + err.Error()
	}
	if !checksumPattern.MatchString(pr.SourceIdentity.Checksum) {
		return fmt.Sprintf("source_identity.checksum %q must match %s", pr.SourceIdentity.Checksum, checksumPattern)
	}
	if pr.Connection.Scheme == "" {
		return "connection.scheme is empty"
	}
	var state map[string]any
	if len(pr.State) == 0 || json.Unmarshal(pr.State, &state) != nil {
		return "state must be a JSON object (§6.2)"
	}
	if pr.SourceIdentity.CreatedAt != nil && !isRFC3339(*pr.SourceIdentity.CreatedAt) {
		return fmt.Sprintf("created_at %q must be null or RFC 3339", *pr.SourceIdentity.CreatedAt)
	}
	return ""
}

// isRFC3339 reports whether s is an RFC 3339 instant. Go's time.RFC3339
// layout is narrower than the standard: RFC 3339 permits lowercase "t" and
// "z" designators, and docs/schemas/adapter/provision-response.json
// declares them valid. Failing an adapter whose output validates against
// the published schema would be a defect in this suite, not in the
// adapter. RFC 3339 contains no other letters, so upper-casing reaches the
// strict layout without changing any value.
func isRFC3339(s string) bool {
	if _, err := time.Parse(time.RFC3339, s); err == nil {
		return true
	}
	_, err := time.Parse(time.RFC3339, strings.ToUpper(s))
	return err == nil
}

func timingsProblem(res *opResult, pr *provisionResult, happyProblem string) string {
	if happyProblem != "" {
		return "unverifiable: provision.happy_path failed"
	}
	t := pr.Timings
	if t.EngineReadySeconds < 0 || t.TransferSeconds < 0 || t.RestoreSeconds < 0 {
		return fmt.Sprintf("negative timings: %+v", t)
	}
	sum := t.EngineReadySeconds + t.TransferSeconds + t.RestoreSeconds
	if wall := res.wall.Seconds(); sum > wall+0.05 {
		return fmt.Sprintf("timings sum %.3fs exceeds the operation's wall clock %.3fs — measure, do not estimate (§7)", sum, wall)
	}
	return ""
}

// checkHealthcheck covers check 11, using the connection and state the
// happy-path provision returned (§6.3: both verbatim).
func (s *suite) checkHealthcheck() {
	if s.provision == nil {
		s.report.add("healthcheck.shape", false, "unverifiable: provision.happy_path failed")
		return
	}
	payload := map[string]any{
		"connection": s.rawConn,
		"state":      s.provision.State,
	}
	res := s.driveOp("healthcheck", payload, driveSpec{})
	problem := ""
	var hc struct {
		Healthy        *bool   `json:"healthy"`
		LatencySeconds float64 `json:"latency_seconds"`
	}
	switch {
	case !res.finalOK():
		problem = fmt.Sprintf("healthcheck failed with code %q; an unhealthy verdict is ok:true (§6.3)", res.finalError())
	case json.Unmarshal(res.final.Payload, &hc) != nil || hc.Healthy == nil:
		problem = "payload is not a §6.3 object with a boolean healthy field"
	case hc.LatencySeconds < 0:
		problem = "latency_seconds is negative"
	}
	s.report.add("healthcheck.shape", problem == "", problem)
}

// checkTeardown covers checks 12–13.
func (s *suite) checkTeardown() {
	empty := map[string]any{"state": map[string]any{}, "reason": "failed"}
	res := s.driveOp("teardown", empty, driveSpec{})
	ok := res.finalOK()
	s.report.add("teardown.empty_state", ok,
		detailUnless(ok, fmt.Sprintf("teardown with state {} failed with code %q — it must cope with the crash case (§6.4)", res.finalError())))

	state := json.RawMessage(`{}`)
	if s.provision != nil {
		state = s.provision.State
	}
	payload := map[string]any{"state": state, "reason": "completed"}
	first := s.driveOp("teardown", payload, driveSpec{})
	second := s.driveOp("teardown", payload, driveSpec{})
	ok = first.finalOK() && second.finalOK()
	s.report.add("teardown.idempotent", ok,
		detailUnless(ok, fmt.Sprintf("consecutive teardowns must both succeed (first code %q, second %q)",
			first.finalError(), second.finalError())))
}

// checkSigterm covers check 14.
func (s *suite) checkSigterm() {
	source, cleanup, err := s.source()
	if err != nil {
		s.err = err
		return
	}
	defer cleanup()

	res := s.driveOp("provision", s.provisionPayload(s.sourceKind(), source),
		driveSpec{sigterm: true, grace: s.opts.Grace})
	problem := ""
	switch {
	case len(res.calls) == 0:
		// Nothing to interrupt: the adapter finished without sandbox calls.
	case res.killed:
		problem = fmt.Sprintf("did not exit within the %s grace period after SIGTERM (§2.4)", s.opts.Grace)
	case res.postSignalCalls > 0:
		problem = fmt.Sprintf("issued %d sandbox call(s) after SIGTERM — §2.4 forbids new calls", res.postSignalCalls)
	case res.final != nil && !res.finalOK() && res.finalError() != "cancelled":
		problem = fmt.Sprintf("final error code %q, want cancelled (§2.4)", res.finalError())
	}
	s.report.add("sigterm.cancels", problem == "", problem)
}

// checkFraming covers check 15 with everything collected along the way.
func (s *suite) checkFraming() {
	if len(s.framing) == 0 {
		s.report.add("framing.discipline", true, "")
		return
	}
	detail := s.framing[0]
	if len(s.framing) > 1 {
		detail = fmt.Sprintf("%s (+%d more)", detail, len(s.framing)-1)
	}
	s.report.add("framing.discipline", false, detail)
}

func (s *suite) sourceKind() string {
	if s.opts.SourceKind != "" {
		return s.opts.SourceKind
	}
	if s.probe != nil && len(s.probe.Sources) > 0 {
		return s.probe.Sources[0].Kind
	}
	return "probavi-conformance-unknown"
}

func (s *suite) provisionPayload(kind, path string) map[string]any {
	params := s.opts.SourceParams
	if params == nil {
		params = map[string]string{}
	}
	return map[string]any{
		"source": map[string]any{
			"kind": kind, "path": path, "params": params, "credential_env": []string{},
		},
		"sandbox": map[string]any{"scratch_dir": "/tmp"},
		"options": map[string]string{},
	}
}

// tempSource creates the generated backup-source file of §10 checks 9–10.
// source returns the artifact to provision from and how to release it:
// the caller's own path when Options names one, and otherwise a temporary
// file the suite made and must delete.
func (s *suite) source() (string, func(), error) {
	if s.opts.SourcePath != "" {
		return s.opts.SourcePath, func() {}, nil
	}
	path, err := tempSource()
	if err != nil {
		return "", func() {}, err
	}
	// Removing the temporary file is best effort: it lives in the
	// system's temp directory and its fate changes no verdict.
	return path, func() { os.Remove(path) }, nil //nolint:errcheck,gosec // best effort
}

func tempSource() (string, error) {
	f, err := os.CreateTemp("", "probavi-conformance-source-*")
	if err != nil {
		return "", fmt.Errorf("create temp source: %w", err)
	}
	buf := make([]byte, 64*1024)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("fill temp source: %w", err)
	}
	if _, err := f.Write(buf); err != nil {
		return "", fmt.Errorf("write temp source: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close temp source: %w", err)
	}
	return f.Name(), nil
}

func requestID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("conformance: crypto/rand unavailable: " + err.Error())
	}
	return "r-" + hex.EncodeToString(b[:])
}

func detailUnless(ok bool, detail string) string {
	if ok {
		return ""
	}
	return detail
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
