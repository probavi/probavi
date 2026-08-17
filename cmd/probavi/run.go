package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/probavi/probavi/internal/adapter"
	"github.com/probavi/probavi/internal/cli"
	"github.com/probavi/probavi/internal/config"
	"github.com/probavi/probavi/internal/conformance"
	"github.com/probavi/probavi/internal/core"
	"github.com/probavi/probavi/internal/evidence"
	"github.com/probavi/probavi/internal/gameday"
	"github.com/probavi/probavi/internal/i18n"
	"github.com/probavi/probavi/internal/metrics"
	"github.com/probavi/probavi/internal/notify"
	"github.com/probavi/probavi/internal/sandbox/docker"
	"github.com/probavi/probavi/internal/sandbox/k8s"
	"github.com/probavi/probavi/internal/sandbox/registry"
	"github.com/probavi/probavi/internal/sandbox/remotehost"
)

// version is stamped into evidence records and printed by `probavi
// version`. Release builds override it with
// -ldflags "-X main.version=<semver>"; anything built without that is a
// dev build and says so.
var version = "0.15.0-dev"

// Exit codes for `probavi run` (cron/CI contract, documented in usage).
// The numbers come from internal/cli, which declares the contract the
// capabilities manifest publishes.
const (
	exitPass         = cli.ExitPass
	exitFail         = cli.ExitFail
	exitError        = cli.ExitError
	exitEvidenceLost = cli.ExitEvidenceLost
)

func runDrill(args []string, stdout, stderr io.Writer, tr *i18n.T) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to the drill configuration YAML (required)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *configPath == "" {
		tr.Fprintf(stderr, msgRunConfigRequired)
		return exitUsage
	}
	logger := slog.New(slog.NewTextHandler(stderr, nil))

	// SIGTERM and Ctrl-C turn into a cancelled drill with a signed record,
	// not a crash.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	summary, code := executeDrill(ctx, *configPath, logger, stderr, "probavi run", tr)
	if code == exitPass || code == exitFail || code == exitError {
		if err := json.NewEncoder(stdout).Encode(summary); err != nil {
			tr.Fprintf(stderr, msgRunEncodeSummary, err)
		}
	}
	return code
}

// executeDrill runs the full drill pipeline for one config file — wiring,
// the drill itself under its configured wall-clock limit, metrics, and
// notifications — and returns the machine summary with the run exit code.
// Both `probavi run` and game-day members go through this path.
func executeDrill(parent context.Context, configPath string, logger *slog.Logger, stderr io.Writer, errPrefix string, tr *i18n.T) (gameday.DrillSummary, int) {
	drill, notifier, evidencePath, cleanup, err := wireDrill(configPath, logger, tr)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", errPrefix, err)
		return gameday.DrillSummary{}, exitUsage
	}
	defer cleanup()

	ctx, cancel := context.WithTimeout(parent, drill.Config.Sandbox.Timeout.Std())
	defer cancel()

	rec, err := drill.Run(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", errPrefix, err)
		return gameday.DrillSummary{}, exitEvidenceLost
	}
	if drill.Config.Metrics != nil {
		// Metrics are observability, not evidence: failures are loud but
		// never change the drill verdict.
		trend, terr := metrics.RestoreTrend(evidencePath, drill.Config.Target.Name)
		if terr != nil {
			logger.Error("compute restore trend", "err", terr)
		}
		if merr := metrics.WriteTextfile(drill.Config.Metrics.PrometheusTextfile, rec, trend); merr != nil {
			logger.Error("write metrics textfile", "err", merr)
		}
	}
	if notifier != nil {
		// Notifications are observability, not evidence: they run on their
		// own budget — deliberately not derived from the drill context, so a
		// cancelled or timed-out drill still notifies — and failures are
		// loud but never change the exit code.
		nctx, ncancel := context.WithTimeout(context.Background(), notify.Budget)
		if nerr := notifier.Send(nctx, rec); nerr != nil {
			logger.Error("deliver notifications", "err", nerr)
		}
		ncancel()
	}
	summary := summarize(rec, evidencePath)
	switch rec.Outcome {
	case evidence.OutcomePass:
		return summary, exitPass
	case evidence.OutcomeFail:
		return summary, exitFail
	default:
		return summary, exitError
	}
}

// runGameDay implements `probavi gameday`: a multi-database restore
// exercise in dependency order (docs/gameday.md).
func runGameDay(args []string, stdout, stderr io.Writer, tr *i18n.T) int {
	fs := flag.NewFlagSet("gameday", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to the game-day configuration YAML (required)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *configPath == "" {
		tr.Fprintf(stderr, msgGameDayConfigRequired)
		return exitUsage
	}
	logger := slog.New(slog.NewTextHandler(stderr, nil))

	cfg, err := config.LoadGameDay(*configPath, tr)
	if err != nil {
		fmt.Fprintf(stderr, "probavi gameday: %v\n", err)
		return exitUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout.Std())
	defer cancel()

	runner := func(ctx context.Context, member config.GameDayMember) gameday.DrillSummary {
		summary, code := executeDrill(ctx, member.Config, logger.With("member", member.Name), stderr,
			"probavi gameday: member "+member.Name, tr)
		switch code {
		case exitUsage:
			return gameday.DrillSummary{Outcome: string(evidence.OutcomeError), ErrorCode: gameday.ErrCodeSetup}
		case exitEvidenceLost:
			return gameday.DrillSummary{Outcome: string(evidence.OutcomeError), ErrorCode: gameday.ErrCodeEvidenceLost}
		default:
			return summary
		}
	}
	summary := gameday.Run(ctx, cfg, runner, logger)
	if err := json.NewEncoder(stdout).Encode(summary); err != nil {
		tr.Fprintf(stderr, msgGameDayEncodeSummary, err)
	}
	return gamedayExit(summary)
}

// gamedayExit maps the summary to the §6 exit codes: a lost evidence
// record dominates everything, then the recoverability verdict.
func gamedayExit(s *gameday.Summary) int {
	for i := range s.Members {
		if s.Members[i].DrillSummary != nil && s.Members[i].ErrorCode == gameday.ErrCodeEvidenceLost {
			return exitEvidenceLost
		}
	}
	switch s.Outcome {
	case string(evidence.OutcomePass):
		return exitPass
	case string(evidence.OutcomeFail):
		return exitFail
	default:
		return exitError
	}
}

// wireDrill builds the object graph for one drill run: config, notifier,
// evidence store, adapter runner, sandbox provider. The notifier is wired
// first so an unresolvable webhook environment variable aborts before any
// long-running work.
func wireDrill(configPath string, logger *slog.Logger, tr *i18n.T) (*core.Drill, *notify.Notifier, string, func(), error) {
	cfg, err := config.Load(configPath, tr)
	if err != nil {
		return nil, nil, "", nil, err
	}
	var notifier *notify.Notifier
	if cfg.Notify != nil {
		notifier, err = notify.New(cfg.Notify, version, logger)
		if err != nil {
			return nil, nil, "", nil, err
		}
	}
	provider, err := sandboxProvider(cfg.Sandbox.Provider, cfg.Sandbox.Params, logger)
	if err != nil {
		return nil, nil, "", nil, err
	}
	signer, err := evidence.LoadSigner(cfg.Evidence.SignKey)
	if err != nil {
		return nil, nil, "", nil, err
	}
	store, err := evidence.Open(cfg.Evidence.Path, signer, logger)
	if err != nil {
		return nil, nil, "", nil, err
	}
	password := randomHex(16)
	runner, err := adapter.New(cfg.Target.Adapter, logger, &adapter.Options{
		CredentialEnv: cfg.Target.Source.CredentialEnv,
		Env:           map[string]string{adapter.SandboxPasswordEnv: password},
	})
	if err != nil {
		if cerr := store.Close(); cerr != nil {
			logger.Error("close evidence store", "err", cerr)
		}
		return nil, nil, "", nil, err
	}
	drill := &core.Drill{
		Config:          cfg,
		Adapter:         runner,
		Provider:        provider,
		Store:           store,
		Logger:          logger,
		Version:         version,
		SandboxPassword: password,
	}
	cleanup := func() {
		if cerr := store.Close(); cerr != nil {
			logger.Error("close evidence store", "err", cerr)
		}
	}
	return drill, notifier, cfg.Evidence.Path, cleanup, nil
}

func summarize(rec *evidence.Record, evidencePath string) gameday.DrillSummary {
	passed := 0
	for _, c := range rec.Checks {
		if c.OK {
			passed++
		}
	}
	s := gameday.DrillSummary{
		Outcome:      string(rec.Outcome),
		Seq:          rec.Seq,
		EvidencePath: evidencePath,
		ChecksPassed: passed,
		ChecksTotal:  len(rec.Checks),
		RestoreMS:    rec.Timings.Restore,
		TotalMS:      rec.Timings.Total,
	}
	if rec.Error != nil {
		s.ErrorCode = rec.Error.Code
	}
	return s
}

// sandboxProvider resolves the drill config's provider name. The params
// are needed at construction time only by remotehost, whose orphan sweep
// runs against the configured workspace root before any Create.
func sandboxProvider(name string, params map[string]string, logger *slog.Logger) (core.Provider, error) {
	switch name {
	case docker.Descriptor.ID:
		return dockerProvider{docker.New(logger)}, nil
	case k8s.Descriptor.ID:
		return k8sProvider{k8s.New(logger)}, nil
	case remotehost.Descriptor.ID:
		p, err := remotehost.New(logger, params)
		if err != nil {
			return nil, err
		}
		return remotehostProvider{p}, nil
	default:
		return nil, fmt.Errorf("unsupported sandbox provider %q (supported: %s)",
			name, strings.Join(registry.IDs(), ", "))
	}
}

// dockerProvider, k8sProvider, and remotehostProvider adapt the concrete
// providers to core.Provider (Go interfaces do not covariantly match the
// concrete sandbox return types).
type dockerProvider struct {
	p *docker.Provider
}

func (d dockerProvider) Create(ctx context.Context, params map[string]string) (core.Sandbox, error) {
	sbx, err := d.p.Create(ctx, params)
	if err != nil {
		return nil, err
	}
	return sbx, nil
}

func (d dockerProvider) SweepOrphans(ctx context.Context) ([]string, error) {
	return d.p.SweepOrphans(ctx)
}

type k8sProvider struct {
	p *k8s.Provider
}

func (k k8sProvider) Create(ctx context.Context, params map[string]string) (core.Sandbox, error) {
	sbx, err := k.p.Create(ctx, params)
	if err != nil {
		return nil, err
	}
	return sbx, nil
}

func (k k8sProvider) SweepOrphans(ctx context.Context) ([]string, error) {
	return k.p.SweepOrphans(ctx)
}

type remotehostProvider struct {
	p *remotehost.Provider
}

func (r remotehostProvider) Create(ctx context.Context, params map[string]string) (core.Sandbox, error) {
	sbx, err := r.p.Create(ctx, params)
	if err != nil {
		return nil, err
	}
	return sbx, nil
}

func (r remotehostProvider) SweepOrphans(ctx context.Context) ([]string, error) {
	return r.p.SweepOrphans(ctx)
}

// runAdapterConformance implements `probavi adapter conformance`: run the
// frozen §10 check list against an adapter and report the verdicts.
func runAdapterConformance(args []string, stdout, stderr io.Writer, tr *i18n.T) int {
	fs := flag.NewFlagSet("adapter conformance", flag.ContinueOnError)
	fs.SetOutput(stderr)
	kind := fs.String("source-kind", "", "source kind for the provision checks (default: the first kind the probe declares)")
	var params stringList
	fs.Var(&params, "source-param", "source parameter as k=v for the provision checks; repeatable")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 || fs.Arg(0) == "" {
		tr.Fprintf(stderr, msgConformanceAdapterRequired)
		return exitUsage
	}
	sourceParams := map[string]string{}
	for _, p := range params {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			tr.Fprintf(stderr, msgConformanceBadSourceParam, p)
			return exitUsage
		}
		sourceParams[k] = v
	}
	path, err := resolveAdapterExecutable(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "probavi adapter conformance: %v\n", err)
		return exitUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	report, err := conformance.Run(ctx, path, conformance.Options{SourceKind: *kind, SourceParams: sourceParams})
	if err != nil {
		fmt.Fprintf(stderr, "probavi adapter conformance: %v\n", err)
		return exitError
	}
	for _, c := range report.Checks {
		if c.OK {
			fmt.Fprintf(stderr, "ok   %s\n", c.Name)
		} else {
			fmt.Fprintf(stderr, "FAIL %s: %s\n", c.Name, c.Detail)
		}
	}
	if err := json.NewEncoder(stdout).Encode(report); err != nil {
		tr.Fprintf(stderr, msgConformanceEncodeReport, err)
		return exitError
	}
	if report.Failed > 0 {
		return exitFail
	}
	return exitPass
}

// resolveAdapterExecutable accepts an adapter name (resolved to
// probavi-adapter-<name> on PATH, §2.1) or an explicit executable path.
func resolveAdapterExecutable(arg string) (string, error) {
	if strings.ContainsRune(arg, os.PathSeparator) {
		path, err := exec.LookPath(arg)
		if err != nil {
			return "", fmt.Errorf("adapter executable %q: %w", arg, err)
		}
		return path, nil
	}
	path, err := exec.LookPath("probavi-adapter-" + arg)
	if err != nil {
		return "", fmt.Errorf("resolve adapter %q: %w", arg, err)
	}
	return path, nil
}

// runAdapterProbe implements `probavi adapter probe <name>`: resolve the
// adapter and print its probe response as JSON.
func runAdapterProbe(args []string, stdout, stderr io.Writer, tr *i18n.T) int {
	if len(args) != 1 || args[0] == "" {
		tr.Fprintf(stderr, msgProbeNameRequired)
		return exitUsage
	}
	logger := slog.New(slog.NewTextHandler(stderr, nil))
	runner, err := adapter.New(args[0], logger, nil)
	if err != nil {
		fmt.Fprintf(stderr, "probavi adapter probe: %v\n", err)
		return exitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	res, err := runner.Probe(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "probavi adapter probe: %v\n", err)
		return exitError
	}
	if err := json.NewEncoder(stdout).Encode(res); err != nil {
		tr.Fprintf(stderr, msgProbeEncode, err)
		return exitError
	}
	return exitPass
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// Unrecoverable: an ephemeral sandbox secret must never be
		// predictable.
		panic("probavi: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}
