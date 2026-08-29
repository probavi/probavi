// Package core orchestrates one drill: sandbox up, adapter-driven restore,
// checks, guaranteed teardown — and exactly one signed evidence record at
// the end, whatever happened in between. A drill that leaves no record is
// the highest-severity bug (evidence-schema.md §7); this package is built
// around that invariant.
package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"runtime"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/probavi/probavi/internal/adapter"
	"github.com/probavi/probavi/internal/checks"
	"github.com/probavi/probavi/internal/config"
	"github.com/probavi/probavi/internal/evidence"
)

// teardownGrace bounds cleanup work that runs after the drill context is
// already dead (PoC finding 3: cleanup never depends on the drill's fate).
const teardownGrace = 60 * time.Second

// AdapterClient is what the core needs from the adapter protocol client.
type AdapterClient interface {
	// Path is the adapter executable this client launches. The core hashes
	// it into adapter.digest so a record says which build produced it
	// (evidence-schema.md §3).
	Path() string
	Probe(ctx context.Context) (*adapter.ProbeResult, error)
	Provision(ctx context.Context, req *adapter.ProvisionRequest, verbs adapter.SandboxVerbs) (*adapter.ProvisionResult, error)
	Healthcheck(ctx context.Context, conn *adapter.Connection, state json.RawMessage, verbs adapter.SandboxVerbs) (*adapter.HealthcheckResult, error)
	Teardown(ctx context.Context, state json.RawMessage, reason string, verbs adapter.SandboxVerbs) (*adapter.TeardownResult, error)
}

// Sandbox is one disposable runtime as the core sees it.
type Sandbox interface {
	adapter.SandboxVerbs
	ID() string
	ScratchDir() string
	Destroy(ctx context.Context) error
}

// Provider creates sandboxes and sweeps the orphans of crashed runs.
type Provider interface {
	Create(ctx context.Context, params map[string]string) (Sandbox, error)
	SweepOrphans(ctx context.Context) ([]string, error)
}

// Appender is the evidence store surface the core needs.
type Appender interface {
	Append(rec *evidence.Record) error
}

// Drill wires one drill run. All collaborators are injected.
type Drill struct {
	Config   *config.Config
	Adapter  AdapterClient
	Provider Provider
	Store    Appender
	Logger   *slog.Logger
	Version  string
	// SandboxPassword is the ephemeral per-drill secret the core generated
	// (adapter protocol §2.5); checks use it when the adapter's connection
	// names it as password_env.
	SandboxPassword string

	Now      func() time.Time
	Hostname func() (string, error)
	// Executable resolves this process's own binary so its digest can be
	// recorded (evidence-schema.md §3). Injected for the same reason
	// Hostname is: a test must not depend on the machine it runs on.
	Executable func() (string, error)
}

// Run executes the drill and appends exactly one signed evidence record.
// The record carries the verdict; the returned error is reserved for the
// one unforgivable failure — the record could not be written.
func (d *Drill) Run(ctx context.Context) (*evidence.Record, error) {
	d.defaults()
	start := d.Now()
	rec := d.baseRecord()

	d.execute(ctx, rec)

	rec.Timings.Total = msSince(start, d.Now())
	rec.TS = d.Now().UTC().Format(evidence.TimestampFormat)
	err := d.Store.Append(rec)
	if err == nil {
		d.Logger.Info("evidence record appended", "seq", rec.Seq, "outcome", rec.Outcome)
		return rec, nil
	}
	if degraded, ok := d.appendDegraded(rec, err); ok {
		return degraded, nil
	}
	return rec, fmt.Errorf("append evidence record (a drill ran but left no evidence — highest severity): %w", err)
}

// rejectedByShape reports whether the store refused a record because of
// what it contains rather than because writing failed. Only then is a
// second attempt worth making: an I/O failure poisons the store, and a
// retry would fail identically.
func rejectedByShape(err error) bool {
	return errors.Is(err, evidence.ErrInvalidRecord) ||
		errors.Is(err, evidence.ErrRecordTooLarge) ||
		errors.Is(err, evidence.ErrNotInteger)
}

// appendDegraded is the last line of defence behind evidence-schema.md §7:
// a drill that ran must leave a record. When the composed record is
// unrepresentable, the alternative to a degraded record is silence — and a
// drill that vanishes from the log is indistinguishable from one that was
// never scheduled, which is exactly the gap an attacker or an unlucky
// operator would want.
//
// The replacement carries only values the core itself produced, so nothing
// an adapter or a database said can make it fail in turn, and it never
// claims a verdict: the outcome is error, because a drill whose result
// could not be represented did not reach a recordable one.
func (d *Drill) appendDegraded(rejected *evidence.Record, cause error) (*evidence.Record, bool) {
	if !rejectedByShape(cause) {
		return nil, false
	}
	cfg := d.Config
	degraded := &evidence.Record{
		Schema: evidence.SchemaID,
		TS:     d.Now().UTC().Format(evidence.TimestampFormat),
		Drill: evidence.Drill{
			Name:       safeText(cfg.Target.Name, "unknown-drill"),
			ConfigHash: cfg.Hash,
			PITRTarget: rejected.Drill.PITRTarget,
		},
		Backup: evidence.Backup{Kind: safeText(cfg.Target.Source.Kind, "unknown")},
		Adapter: evidence.Adapter{
			Name:     safeText(cfg.Target.Adapter, "unknown"),
			Protocol: adapter.ProtocolVersion,
			Digest:   rejected.Adapter.Digest,
		},
		// Sandbox params are operator data copied verbatim and are the most
		// likely reason a record is unrepresentable; a degraded record drops
		// them rather than risking a second rejection for the same cause.
		Sandbox: evidence.Sandbox{
			Provider: safeText(cfg.Sandbox.Provider, "unknown"),
			Params:   map[string]string{},
		},
		Checks:  []evidence.Check{},
		Timings: evidence.Timings{Total: rejected.Timings.Total},
		Outcome: evidence.OutcomeError,
		Error: &evidence.DrillError{
			Code: "internal",
			Message: sanitizeMessage(fmt.Sprintf(
				"evidence record replaced: the composed record was rejected (%v); the drill reached outcome %q",
				cause, rejected.Outcome)),
		},
		Env: evidence.Env{
			ProbaviVersion: safeText(d.Version, "unknown"),
			OS:             runtime.GOOS,
			Arch:           runtime.GOARCH,
			HostID:         d.hostID(),
			ProbaviDigest:  rejected.Env.ProbaviDigest,
		},
	}
	if err := d.Store.Append(degraded); err != nil {
		d.Logger.Error("degraded evidence record also rejected", "err", err)
		return nil, false
	}
	d.Logger.Error("composed evidence record was rejected; a degraded record was written instead — this is a bug, please report it",
		"seq", degraded.Seq, "rejected_outcome", rejected.Outcome, "cause", cause)
	return degraded, true
}

func (d *Drill) defaults() {
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Hostname == nil {
		d.Hostname = os.Hostname
	}
	if d.Executable == nil {
		d.Executable = os.Executable
	}
}

// baseRecord pre-fills everything knowable before the drill starts, so any
// abort still produces a complete, valid record with nulls for the unknown.
// A PITR drill's recovery target is resolved to an absolute instant here,
// once: the same value goes into the record and to the adapter, and it is
// recorded even when the drill aborts before provisioning.
func (d *Drill) baseRecord() *evidence.Record {
	cfg := d.Config
	params := cfg.Sandbox.Params
	if params == nil {
		params = map[string]string{}
	}
	var pitrTarget *string
	if cfg.Target.PITR != nil {
		resolved := cfg.Target.PITR.Resolve(d.Now()).UTC().Format(evidence.TimestampFormat)
		pitrTarget = &resolved
	}
	return &evidence.Record{
		Schema: evidence.SchemaID,
		Drill:  evidence.Drill{Name: cfg.Target.Name, ConfigHash: cfg.Hash, PITRTarget: pitrTarget},
		Backup: evidence.Backup{Kind: cfg.Target.Source.Kind},
		Adapter: evidence.Adapter{
			Name:     cfg.Target.Adapter,
			Protocol: adapter.ProtocolVersion,
			Digest:   evidence.FileDigest(d.Adapter.Path()),
		},
		Sandbox: evidence.Sandbox{Provider: cfg.Sandbox.Provider, Params: params},
		Checks:  []evidence.Check{},
		Env: evidence.Env{
			ProbaviVersion: d.Version,
			OS:             runtime.GOOS,
			Arch:           runtime.GOARCH,
			HostID:         d.hostID(),
			ProbaviDigest:  d.selfDigest(),
		},
	}
}

// execute runs the drill phases and fills rec with outcome, error, checks,
// and timings. It never returns an error: every failure becomes record
// content.
func (d *Drill) execute(ctx context.Context, rec *evidence.Record) {
	if removed, err := d.Provider.SweepOrphans(ctx); err != nil {
		d.Logger.Warn("orphan sweep failed", "err", err)
	} else if len(removed) > 0 {
		d.Logger.Info("swept orphan sandboxes", "removed", removed)
	}

	probe, err := d.Adapter.Probe(ctx)
	if err != nil {
		d.classify(ctx, rec, err)
		return
	}
	rec.Adapter.Version = &probe.AdapterVersion
	if !supportsKind(probe, d.Config.Target.Source.Kind) {
		rec.Outcome = evidence.OutcomeError
		rec.Error = &evidence.DrillError{Code: "unsupported_source",
			Message: fmt.Sprintf("adapter %s does not support source kind %s", probe.Name, d.Config.Target.Source.Kind)}
		return
	}
	// The protocol (§6.2) forbids sending pitr to a source kind that did not
	// declare the capability; gate here so the config error is precise.
	if d.Config.Target.PITR != nil && !supportsPITR(probe, d.Config.Target.Source.Kind) {
		rec.Outcome = evidence.OutcomeError
		rec.Error = &evidence.DrillError{Code: "unsupported_source",
			Message: fmt.Sprintf("source kind %s does not support point-in-time recovery (adapter %s)", d.Config.Target.Source.Kind, probe.Name)}
		return
	}

	provisionStart := d.Now()
	sbx, err := d.Provider.Create(ctx, d.Config.Sandbox.Params)
	if err != nil {
		d.classify(ctx, rec, fmt.Errorf("create sandbox: %w", err))
		if rec.Error != nil && rec.Error.Code == "internal" {
			rec.Error.Code = "sandbox_error"
		}
		return
	}
	rec.Timings.Provision = msSince(provisionStart, d.Now())
	defer d.destroySandbox(sbx)

	provRes, perr := d.Adapter.Provision(ctx, d.provisionRequest(sbx, rec.Drill.PITRTarget), sbx)
	defer d.teardown(rec, provRes, sbx)
	if perr != nil {
		d.classify(ctx, rec, perr)
		return
	}
	recordProvision(rec, provRes)

	validateStart := d.Now()
	results, cerr := checks.Run(ctx, d.Config.Checks, d.checkDeps(probe, provRes, sbx))
	rec.Checks = mapChecks(results)
	rec.Timings.Validate = msSince(validateStart, d.Now())
	if cerr != nil {
		d.classify(ctx, rec, cerr)
		return
	}
	if failed := countFailed(results); failed > 0 {
		rec.Outcome = evidence.OutcomeFail
		rec.Error = &evidence.DrillError{Code: "check_failed",
			Message: fmt.Sprintf("%d of %d checks failed", failed, len(results))}
		return
	}
	rec.Outcome = evidence.OutcomePass
}

func (d *Drill) provisionRequest(sbx Sandbox, pitrTarget *string) *adapter.ProvisionRequest {
	src := d.Config.Target.Source
	req := &adapter.ProvisionRequest{
		Source: adapter.ProvisionSource{
			Kind:          src.Kind,
			Path:          src.Path,
			Params:        src.Params,
			CredentialEnv: src.CredentialEnv,
		},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
		Options: d.Config.Target.Options,
	}
	if pitrTarget != nil {
		req.PITR = &adapter.PITR{TargetTime: *pitrTarget}
	}
	return req
}

func (d *Drill) checkDeps(probe *adapter.ProbeResult, provRes *adapter.ProvisionResult, sbx Sandbox) checks.Deps {
	return checks.Deps{
		Exec: sbx,
		Healthcheck: func(hctx context.Context) (bool, string, error) {
			res, err := d.Adapter.Healthcheck(hctx, &provRes.Connection, provRes.State, sbx)
			if err != nil {
				return false, "", err
			}
			return res.Healthy, res.Detail, nil
		},
		Runner: checks.Runner{Argv: probe.SQLRunner.Argv, Env: probe.SQLRunner.Env},
		Target: checks.Target{
			User:     provRes.Connection.User,
			Database: provRes.Connection.Database,
			Password: d.resolvePassword(provRes.Connection.PasswordEnv),
		},
		Now:    d.Now,
		Logger: d.Logger,
	}
}

// resolvePassword turns a connection's password_env NAME into its value:
// the core-generated ephemeral secret, or a variable the drill config
// declared in source.credential_env.
//
// The name comes from the adapter, so the set of variables it may reach
// has to be the same allow-list the adapter's own environment is built
// from (protocol §2.5). Reading any variable the core process happens to
// hold would let an adapter name, say, AWS_SECRET_ACCESS_KEY and have the
// core hand it to a process the adapter controls inside the sandbox —
// exfiltration through a field meant for a database password.
func (d *Drill) resolvePassword(passwordEnv string) string {
	switch {
	case passwordEnv == "":
		return ""
	case passwordEnv == adapter.SandboxPasswordEnv:
		return d.SandboxPassword
	case slices.Contains(d.Config.Target.Source.CredentialEnv, passwordEnv):
		v, _ := os.LookupEnv(passwordEnv)
		return v
	default:
		d.Logger.Warn("adapter asked for an environment variable the drill did not declare; password_env ignored",
			"password_env", passwordEnv, "declared", d.Config.Target.Source.CredentialEnv)
		return ""
	}
}

// teardown always runs after a provision attempt — crash included — on a
// fresh context with its own budget (§2.4, §6.4).
func (d *Drill) teardown(rec *evidence.Record, provRes *adapter.ProvisionResult, sbx Sandbox) {
	tctx, cancel := context.WithTimeout(context.Background(), teardownGrace)
	defer cancel()
	var state json.RawMessage
	if provRes != nil {
		state = provRes.State
	}
	if _, err := d.Adapter.Teardown(tctx, state, teardownReason(rec), sbx); err != nil {
		d.Logger.Error("adapter teardown failed", "err", err)
	}
}

func (d *Drill) destroySandbox(sbx Sandbox) {
	dctx, cancel := context.WithTimeout(context.Background(), teardownGrace)
	defer cancel()
	if err := sbx.Destroy(dctx); err != nil {
		d.Logger.Error("sandbox destroy failed — the next run's orphan sweep will retry", "id", sbx.ID(), "err", err)
	}
}

func teardownReason(rec *evidence.Record) string {
	switch {
	case rec.Outcome == evidence.OutcomePass:
		return "completed"
	case rec.Outcome == evidence.OutcomeCancelled:
		return "cancelled"
	case rec.Error != nil && rec.Error.Code == "timeout":
		return "timeout"
	default:
		return "failed"
	}
}

// failCodes are the recoverability verdicts of evidence-schema.md §7: the
// backup or restore is the problem, not the infrastructure.
var failCodes = map[string]bool{
	evidence.CodeSourceNotFound:   true,
	evidence.CodeSourceUnreadable: true,
	evidence.CodeSourceCorrupt:    true,
	evidence.CodeRestoreFailed:    true,
	evidence.CodeCheckFailed:      true,
}

// classify converts any drill-phase failure into record content following
// the §7 outcome taxonomy.
func (d *Drill) classify(ctx context.Context, rec *evidence.Record, err error) {
	code, message := evidence.CodeInternal, err.Error()
	var aerr *adapter.Error
	if errors.As(err, &aerr) {
		code, message = aerr.Code, aerr.Message
	}
	// The drill's own context outranks whatever the adapter managed to say
	// on its way down. An adapter killed by a deadline or a signal usually
	// reports adapter_crash — true of the process, and a lie about the
	// drill: the record would blame a third party's adapter for the
	// operator's Ctrl-C, in a document written to be read by an auditor.
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		code = evidence.CodeTimeout
		message = "drill wall-clock limit exceeded: " + message
	case errors.Is(ctx.Err(), context.Canceled):
		code = evidence.CodeCancelled
		message = "drill cancelled: " + message
	}
	// The adapter chooses its own code, and a record carrying one outside
	// the published vocabulary would verify as VALID while failing the
	// schema every consumer validates against — a contradiction a trust
	// product cannot ship. Map it to internal and keep the original where
	// it stays readable.
	if !evidence.IsErrorCode(code) {
		message = fmt.Sprintf("adapter %s reported unregistered error code %q: %s",
			d.Config.Target.Adapter, code, message)
		code = evidence.CodeInternal
	}
	message = sanitizeMessage(message)
	switch {
	case failCodes[code]:
		rec.Outcome = evidence.OutcomeFail
	case code == evidence.CodeCancelled:
		rec.Outcome = evidence.OutcomeCancelled
	default:
		rec.Outcome = evidence.OutcomeError
	}
	rec.Error = &evidence.DrillError{Code: code, Message: message}
	d.Logger.Error("drill did not pass", "code", code, "outcome", rec.Outcome)
}

func recordProvision(rec *evidence.Record, res *adapter.ProvisionResult) {
	rec.Backup.Checksum = &res.SourceIdentity.Checksum
	rec.Backup.SizeBytes = &res.SourceIdentity.SizeBytes
	rec.Backup.CreatedAt = res.SourceIdentity.CreatedAt
	rec.Timings.EngineReady = secondsToMS(res.Timings.EngineReadySeconds)
	rec.Timings.Transfer = secondsToMS(res.Timings.TransferSeconds)
	rec.Timings.Restore = secondsToMS(res.Timings.RestoreSeconds)
}

func mapChecks(results []checks.Result) []evidence.Check {
	out := make([]evidence.Check, 0, len(results))
	for _, r := range results {
		c := evidence.Check{Name: r.Name, OK: r.OK}
		if r.Detail != "" {
			detail := r.Detail
			c.Detail = &detail
		}
		out = append(out, c)
	}
	return out
}

func countFailed(results []checks.Result) int {
	failed := 0
	for _, r := range results {
		if !r.OK {
			failed++
		}
	}
	return failed
}

func supportsKind(probe *adapter.ProbeResult, kind string) bool {
	for _, s := range probe.Sources {
		if s.Kind == kind {
			return true
		}
	}
	return false
}

func supportsPITR(probe *adapter.ProbeResult, kind string) bool {
	for _, s := range probe.Sources {
		if s.Kind == kind {
			return s.Capabilities.PITR
		}
	}
	return false
}

// selfDigest is the sha256 reference of the probavi binary that is writing
// the record. A path this process cannot resolve or read yields null, the
// same as any other unreadable executable: §3 makes the field nullable so
// that build identity never costs a drill its signed record.
func (d *Drill) selfDigest() *string {
	path, err := d.Executable()
	if err != nil {
		return nil
	}
	return evidence.FileDigest(path)
}

func (d *Drill) hostID() string {
	name, err := d.Hostname()
	if err != nil {
		name = "unknown-host"
	}
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])[:16]
}

// secondsToMS converts adapter-reported seconds to the schema's integer
// milliseconds, rounding half away from zero (evidence-schema.md §3.1).
func secondsToMS(s float64) *int64 {
	ms := int64(math.Round(s * 1000))
	return &ms
}

func msSince(start, now time.Time) *int64 {
	ms := now.Sub(start).Milliseconds()
	return &ms
}

// safeText returns s when it can appear in a record as-is, and the given
// substitute otherwise. It exists for the degraded record only: that
// record's whole purpose is to be constructible when the normal one was
// not, so every string it carries has to be checked rather than assumed.
func safeText(s, substitute string) string {
	if s == "" || !utf8.ValidString(s) || strings.ContainsAny(s, "\r\n") || len(s) > 256 {
		return substitute
	}
	return s
}

// sanitizeMessage keeps failure text single-line and within the evidence
// limit; quotes are already avoided by the protocol layers.
//
// The cap is measured in bytes by the record layer, so it must be applied
// in bytes here. Counting runes instead let 400 characters of ordinary
// accented text — an adapter reporting an engine error in its own language
// — exceed a 512-byte limit and make the record unwritable, losing the
// evidence for a drill that had already run.
func sanitizeMessage(s string) string {
	out := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, s)
	return evidence.TruncateLine(out, evidence.MaxErrorMessageBytes)
}
