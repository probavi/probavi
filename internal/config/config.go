// Package config loads and validates Probavi drill configurations (the
// YAML "drill as code" files). Validation is strict — unknown fields,
// duplicate keys, and misconfigured checks are errors — and reports every
// problem it finds in one pass, not just the first.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/probavi/probavi/internal/i18n"
)

var (
	adapterNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	envNamePattern     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Config is one drill definition: restore one backup into one sandbox and
// validate it. Hash and Path are filled by Load, never by YAML.
type Config struct {
	Target   Target   `yaml:"target"`
	Sandbox  Sandbox  `yaml:"sandbox"`
	Checks   []Check  `yaml:"checks"`
	Evidence Evidence `yaml:"evidence"`
	Metrics  *Metrics `yaml:"metrics"`
	Notify   *Notify  `yaml:"notify"`

	// Hash is "sha256:<hex>" over the exact file bytes as read — the value
	// evidence records carry as drill.config_hash.
	Hash string `yaml:"-"`
	// Path is the config file path Load read, for error messages.
	Path string `yaml:"-"`
}

// Target names the database under drill and the backup source to restore.
type Target struct {
	Name    string            `yaml:"name"`
	Adapter string            `yaml:"adapter"`
	Source  Source            `yaml:"source"`
	Options map[string]string `yaml:"options"`
	PITR    *PITR             `yaml:"pitr"`
}

// PITR requests point-in-time recovery. Exactly one of TargetTime (an
// absolute RFC 3339 instant) or TargetAge (a relative age the core resolves
// to now−age at drill start, so scheduled drills never go stale) must be
// set. Time is the only engine-neutral recovery target the core schema
// knows (AGENTS.md §6, decided 2026-08-01); engine-specific coordinates
// belong in source.params.
type PITR struct {
	TargetTime string   `yaml:"target_time"`
	TargetAge  Duration `yaml:"target_age"`

	// parsedTime caches the validated TargetTime so Resolve needs no
	// second, error-swallowing parse.
	parsedTime time.Time
}

// Source describes the backup source; Kind is adapter-defined and Params
// pass through the core uninterpreted (adapter protocol §6.2).
type Source struct {
	Kind          string            `yaml:"kind"`
	Path          string            `yaml:"path"`
	Params        map[string]string `yaml:"params"`
	CredentialEnv []string          `yaml:"credential_env"`
}

// Sandbox selects the disposable runtime; Params are provider-specific and
// pass through the core uninterpreted.
type Sandbox struct {
	Provider string            `yaml:"provider"`
	Params   map[string]string `yaml:"params"`
	Timeout  Duration          `yaml:"timeout"`
}

// Check is one validation to run against the restored database: exactly one
// of Builtin or SQL must be set.
type Check struct {
	Builtin string   `yaml:"builtin"`
	Table   string   `yaml:"table"`
	Column  string   `yaml:"column"`
	Min     *int64   `yaml:"min"`
	Max     *int64   `yaml:"max"`
	MaxAge  Duration `yaml:"max_age"`

	Name   string `yaml:"name"`
	SQL    string `yaml:"sql"`
	Expect Scalar `yaml:"expect"`
}

// Evidence configures where records are appended and which key signs them.
type Evidence struct {
	Path    string `yaml:"path"`
	SignKey string `yaml:"sign_key"`
}

// Metrics configures optional metrics exposition.
type Metrics struct {
	PrometheusTextfile string `yaml:"prometheus_textfile"`
}

// Notify configures optional drill-completion notifications
// (docs/notifications.md).
type Notify struct {
	Webhooks []NotifyWebhook `yaml:"webhooks"`
}

// NotifyWebhook is one webhook destination. Exactly one of URL (a
// non-secret literal) or URLEnv (the name of an environment variable
// holding the URL) must be set — token-bearing URLs are credentials and
// belong in the environment, never in config values.
type NotifyWebhook struct {
	URL       string   `yaml:"url"`
	URLEnv    string   `yaml:"url_env"`
	SecretEnv string   `yaml:"secret_env"`
	On        []string `yaml:"on"`
}

// notifyOutcomes are the outcome names a webhook's on filter may list
// (docs/notifications.md §2); they mirror the evidence outcome values.
var notifyOutcomes = map[string]bool{
	"pass": true, "fail": true, "error": true, "cancelled": true,
}

// NotifyOutcomes returns the outcome names a webhook's on filter accepts,
// sorted. It is derived from the validation table above, so the generated
// capabilities manifest cannot disagree with what a config may contain.
func NotifyOutcomes() []string {
	out := make([]string, 0, len(notifyOutcomes))
	for o := range notifyOutcomes {
		out = append(out, o)
	}
	sort.Strings(out)
	return out
}

// Load reads, parses, and validates a drill configuration. The returned
// error is human-oriented — syntax errors carry line/column context and
// an annotated source excerpt, validation reports every problem found —
// and its diagnostics speak the translator's language (docs/i18n.md;
// YAML-level messages remain English).
func Load(path string, tr *i18n.T) (*Config, error) {
	if tr == nil {
		tr = i18n.English()
	}
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, errorf(tr, msgReadConfig, err)
	}
	cfg := &Config{}
	if err := decodeStrict(raw, cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errorf(tr, msgConfigEmpty, path)
		}
		return nil, errorf(tr, msgParseConfig, path, yaml.FormatError(err, false, true))
	}
	sum := sha256.Sum256(raw)
	cfg.Hash = "sha256:" + hex.EncodeToString(sum[:])
	cfg.Path = path
	if err := cfg.validate(tr); err != nil {
		return nil, errorf(tr, msgInvalidConfig, path, err)
	}
	return cfg, nil
}

// decodeStrict decodes a YAML document into v with unknown fields and
// duplicate keys refused, and turns a decoder panic into an error.
//
// The recover is not defensive habit. The decoder is a dependency, and on
// the pinned version it panics rather than reporting on some malformed
// documents — measured: a tag on an empty node where a sequence belongs
// (`checks: !x `) dereferences nil inside its own AST walk, and the
// game-day loader crashes the same way on `depends_on: !x `. A config
// file is operator input read before anything else runs, so a crash there
// is a drill that reports nothing at all: no diagnostic, no record, and
// an exit code that says the binary died rather than that the config is
// wrong.
//
// It is scoped to this one call and the recovered value is carried into
// the message, so a panic from this package's own unmarshalers — which
// this necessarily also catches — surfaces as text somebody can read
// rather than as silence. The fuzz targets in this package are what keep
// watching for both.
func decodeStrict(raw []byte, v any) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("the YAML decoder could not read this document: %v", r)
		}
	}()
	return yaml.NewDecoder(bytes.NewReader(raw), yaml.Strict()).Decode(v)
}

// errorf builds a translated error. The no-argument path of Sprintf
// returns the (translated) format verbatim, so verbs — including %w —
// survive intact; verb parity per catalog is CI-gated.
func errorf(tr *i18n.T, format string, a ...any) error {
	return fmt.Errorf(tr.Sprintf(format), a...)
}

// problems collects validation errors so a config author sees everything
// wrong at once instead of fixing one field per run; diagnostics are
// translated as they are recorded.
type problems struct {
	tr   *i18n.T
	errs []error
}

func (p *problems) add(format string, a ...any) {
	p.errs = append(p.errs, errorf(p.tr, format, a...))
}

func (c *Config) validate(tr *i18n.T) error {
	p := problems{tr: tr}
	c.Target.validate(&p)
	c.Sandbox.validate(&p)
	c.validateChecks(&p)
	c.Evidence.validate(&p)
	if c.Metrics != nil && c.Metrics.PrometheusTextfile == "" {
		p.add(msgMetricsTextfile)
	}
	if c.Notify != nil {
		c.Notify.validate(&p)
	}
	return errors.Join(p.errs...)
}

func (n *Notify) validate(p *problems) {
	if len(n.Webhooks) == 0 {
		p.add(msgNotifyWebhooksRequired)
		return
	}
	for i := range n.Webhooks {
		n.Webhooks[i].validate(p, i)
	}
}

func (w *NotifyWebhook) validate(p *problems, i int) {
	at := fmt.Sprintf("notify.webhooks[%d]", i)
	switch {
	case w.URL != "" && w.URLEnv != "":
		p.add(msgWebhookURLNotBoth, at)
	case w.URLEnv != "":
		if !envNamePattern.MatchString(w.URLEnv) {
			p.add(msgWebhookURLEnvName, at, w.URLEnv)
		}
	case w.URL != "":
		if u, err := url.Parse(w.URL); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			p.add(msgWebhookURLShape, at)
		}
	default:
		p.add(msgWebhookURLNeither, at)
	}
	if w.SecretEnv != "" && !envNamePattern.MatchString(w.SecretEnv) {
		p.add(msgWebhookSecretEnvName, at, w.SecretEnv)
	}
	seen := make(map[string]bool, len(w.On))
	for _, o := range w.On {
		switch {
		case !notifyOutcomes[o]:
			p.add(msgWebhookUnknownOutcome, at, o)
		case seen[o]:
			p.add(msgWebhookDuplicateOutcome, at, o)
		}
		seen[o] = true
	}
}

func (t *Target) validate(p *problems) {
	if t.Name == "" {
		p.add(msgTargetNameRequired)
	}
	switch {
	case t.Adapter == "":
		p.add(msgAdapterRequired)
	case !adapterNamePattern.MatchString(t.Adapter):
		p.add(msgAdapterNamePattern, t.Adapter, t.Adapter)
	}
	if t.Source.Kind == "" {
		p.add(msgSourceKindRequired)
	}
	for _, name := range t.Source.CredentialEnv {
		if !envNamePattern.MatchString(name) {
			p.add(msgCredentialEnvName, name)
		}
	}
	if t.PITR != nil {
		t.PITR.validate(p)
	}
}

func (pt *PITR) validate(p *problems) {
	hasTime := pt.TargetTime != ""
	hasAge := pt.TargetAge != 0
	if hasTime == hasAge {
		p.add(msgPITRExactlyOne)
		return
	}
	if hasTime {
		ts, err := time.Parse(time.RFC3339, pt.TargetTime)
		if err != nil {
			p.add(msgPITRBadTargetTime, pt.TargetTime)
			return
		}
		if ts.After(time.Now().Add(pitrClockSkewGrace)) {
			p.add(msgPITRFutureTargetTime, pt.TargetTime)
			return
		}
		pt.parsedTime = ts
	}
}

// pitrClockSkewGrace is how far ahead of this host's clock a target_time
// may sit before it is called a mistake. A drill can only prove recovery
// to an instant that has happened; an engine handed a future target simply
// recovers as far as it can, so the config is asking for something other
// than what it says. The grace absorbs ordinary skew between the host that
// wrote the config and the one running the drill, without letting through
// the mistake this catches — a year or a month typed wrong.
const pitrClockSkewGrace = time.Minute

// Resolve returns the absolute recovery target: the validated target_time,
// or now minus target_age. Only meaningful on a Config returned by Load.
func (pt *PITR) Resolve(now time.Time) time.Time {
	if !pt.parsedTime.IsZero() {
		return pt.parsedTime
	}
	return now.Add(-pt.TargetAge.Std())
}

func (s *Sandbox) validate(p *problems) {
	if s.Provider == "" {
		p.add(msgProviderRequired)
	}
	if s.Timeout == 0 {
		p.add(msgTimeoutRequired)
	}
}

func (c *Config) validateChecks(p *problems) {
	if len(c.Checks) == 0 {
		p.add(msgChecksRequired)
		return
	}
	for i := range c.Checks {
		c.Checks[i].validate(p, i)
	}
}

func (e *Evidence) validate(p *problems) {
	if e.Path == "" {
		p.add(msgEvidencePathRequired)
	}
	if e.SignKey == "" {
		p.add(msgSignKeyRequired)
	}
}

func (ch *Check) validate(p *problems, i int) {
	at := fmt.Sprintf("checks[%d]", i)
	switch {
	case ch.Builtin != "" && ch.SQL != "":
		p.add(msgCheckBuiltinOrSQLNotBoth, at)
	case ch.Builtin != "":
		ch.validateBuiltin(p, at)
	case ch.SQL != "":
		ch.validateSQL(p, at)
	default:
		p.add(msgCheckBuiltinOrSQL, at)
	}
}

func (ch *Check) validateBuiltin(p *problems, at string) {
	if ch.Expect.IsSet() {
		p.add(msgCheckExpectOnlySQL, at)
	}
	if ch.Name != "" {
		p.add(msgCheckNameOnlySQL, at)
	}
	// The registry (builtins.go) is the vocabulary gate: a kind absent
	// from it is rejected here and therefore never reaches internal/checks.
	kind, ok := LookupCheckKind(ch.Builtin)
	if !ok || !kind.Builtin {
		p.add(msgCheckUnknownBuiltin, at, ch.Builtin)
		return
	}
	switch kind.ID {
	case CheckServiceHealthy:
		ch.forbid(p, at, fields{table: true, column: true, minmax: true, maxAge: true})
	case CheckTableExists:
		ch.requireTable(p, at)
		ch.forbid(p, at, fields{column: true, minmax: true, maxAge: true})
	case CheckRowCount:
		ch.validateRowCount(p, at)
	case CheckFreshness:
		ch.validateFreshness(p, at)
	default:
		// A registered built-in with no rules here is a defect in this
		// package, not a user error; TestEveryBuiltinIsValidated pins it.
		p.add(msgCheckUnknownBuiltin, at, ch.Builtin)
	}
}

func (ch *Check) validateRowCount(p *problems, at string) {
	ch.requireTable(p, at)
	ch.forbid(p, at, fields{column: true, maxAge: true})
	switch {
	case ch.Min == nil && ch.Max == nil:
		p.add(msgCheckRowCountBounds, at)
	case ch.Min != nil && *ch.Min < 0, ch.Max != nil && *ch.Max < 0:
		p.add(msgCheckRowCountNegative, at)
	case ch.Min != nil && ch.Max != nil && *ch.Min > *ch.Max:
		p.add(msgCheckRowCountMinMax, at, *ch.Min, *ch.Max)
	}
}

func (ch *Check) validateFreshness(p *problems, at string) {
	ch.requireTable(p, at)
	ch.forbid(p, at, fields{minmax: true})
	if ch.Column == "" {
		p.add(msgCheckFreshnessColumn, at)
	}
	if ch.MaxAge == 0 {
		p.add(msgCheckFreshnessMaxAge, at)
	}
}

func (ch *Check) validateSQL(p *problems, at string) {
	if !ch.Expect.IsSet() {
		p.add(msgCheckSQLExpect, at)
	}
	ch.forbid(p, at, fields{table: true, column: true, minmax: true, maxAge: true})
}

// fields marks which check parameters are not applicable in a context.
type fields struct {
	table, column, minmax, maxAge bool
}

func (ch *Check) requireTable(p *problems, at string) {
	if ch.Table == "" {
		p.add(msgCheckRequiresTable, at, ch.Builtin)
	}
}

func (ch *Check) forbid(p *problems, at string, f fields) {
	kind := ch.Builtin
	if kind == "" {
		kind = "sql checks"
	}
	if f.table && ch.Table != "" {
		p.add(msgCheckTableNotValid, at, kind)
	}
	if f.column && ch.Column != "" {
		p.add(msgCheckColumnNotValid, at, kind)
	}
	if f.minmax && (ch.Min != nil || ch.Max != nil) {
		p.add(msgCheckMinMaxNotValid, at, kind)
	}
	if f.maxAge && ch.MaxAge != 0 {
		p.add(msgCheckMaxAgeNotValid, at, kind)
	}
}
