package main

// messages.go declares every translatable user-facing string
// (docs/i18n.md §4). Constant values double as catalog keys — the key
// is the English text itself — and allMessages drives the per-locale
// completeness, staleness, and format-verb-parity gates in the test
// suite. Wrapper lines of the form "probavi run: %v" carry English
// error chains from internal packages and stay untranslated until the
// validation-diagnostics step (ROADMAP Phase 4).
const (
	msgUnknownCommand     = "probavi: unknown command %q\n\n"
	msgUnknownAdapterSub  = "probavi adapter: unknown subcommand %q\n\n"
	msgUnknownEvidenceSub = "probavi evidence: unknown subcommand %q\n\n"

	msgRunConfigRequired = "probavi run: --config is required\n"
	msgRunEncodeSummary  = "probavi run: encode summary: %v\n"

	msgGameDayConfigRequired = "probavi gameday: --config is required\n"
	msgGameDayEncodeSummary  = "probavi gameday: encode summary: %v\n"

	msgVerifyFlagsRequired = "probavi evidence verify: --log and at least one --key are required\n"
	msgVerifyEncodeResult  = "probavi evidence verify: encode result: %v\n"
	// An intact log with nothing in it is the one verdict a reader can
	// misread as good news: it exits 0, exactly like a log of verified
	// drills. The exit code is normative (evidence schema §9) and stays
	// as it is; the difference is stated instead.
	msgVerifyNoRecords     = "probavi evidence verify: the log is intact but holds no records — nothing has been proven, and this exits 0 exactly as a log of verified drills does\n"
	msgKeygenOutRequired   = "probavi evidence keygen: --out is required\n"
	msgKeygenEncodeResult  = "probavi evidence keygen: encode result: %v\n"

	msgConformanceAdapterRequired = "probavi adapter conformance: exactly one adapter name or executable path is required\n"
	msgConformanceBadSourceParam  = "probavi adapter conformance: --source-param %q is not k=v\n"
	msgConformanceEncodeReport    = "probavi adapter conformance: encode report: %v\n"
	msgProbeNameRequired          = "probavi adapter probe: exactly one adapter name is required\n"
	msgProbeEncode                = "probavi adapter probe: encode: %v\n"

	msgVersionProtocol = "adapter protocol: %s\n"
	msgVersionSchema   = "evidence schema:  %s (verifies all published versions)\n"

	msgUsage = `Usage: probavi <command> [arguments]

Commands:
  run --config <drill.yaml>
      Execute one restore drill: sandbox up, restore, checks, teardown,
      and exactly one signed evidence record. Prints a one-line JSON
      summary on stdout. Run it from cron or a systemd timer — Probavi
      deliberately has no built-in scheduler.
      Exit codes: 0 backup proven restorable, 1 recoverability failure
      (backup/restore/check), 2 infrastructure error or cancelled,
      3 usage or setup error, 5 evidence record could not be written.

  gameday --config <gameday.yaml>
      Execute a DR game-day: member drills in dependency order, each the
      full run pipeline with its own signed evidence record; dependents
      of a failed member are skipped, independent branches continue.
      Prints a one-line JSON summary on stdout (docs/gameday.md).
      Exit codes: 0 every member passed, 1 a member drill failed,
      2 errors/cancellation left members unproven, 3 usage or setup
      error, 5 a member's evidence record could not be written.

  evidence verify --log <file> --key <pubkey> [--key <pubkey> ...]
      Verify an evidence log offline against one or more public keys.
      Prints a one-line JSON result on stdout.
      Exit codes: 0 VALID, 1 VALID_WITH_DAMAGE, 2 INVALID,
      3 usage or I/O error.

  evidence keygen --out <path>
      Generate an ed25519 signing key pair: <path> (mode 0600) and
      <path>.pub. Refuses to overwrite existing files.

  adapter probe <name>
      Resolve probavi-adapter-<name> and print its capabilities as JSON.

  adapter conformance [--source-kind <kind>] [--source-param k=v ...] <name-or-path>
      Drive the adapter through the frozen protocol conformance checks
      (docs/adapter-protocol.md §10) against a simulated sandbox — no
      container runtime involved. A new adapter is done when this passes.
      Prints one line per check on stderr and a JSON report on stdout.
      Exit codes: 0 conformant, 1 one or more checks failed, 2 the suite
      could not be run to completion, 3 usage error.

  version
      Print the probavi version and the contract versions this build
      speaks (adapter protocol, evidence schema).
`
)

// allMessages is the complete translatable surface; the i18n gates
// iterate it (docs/i18n.md §4).
var allMessages = []string{
	msgUnknownCommand,
	msgUnknownAdapterSub,
	msgUnknownEvidenceSub,
	msgRunConfigRequired,
	msgRunEncodeSummary,
	msgGameDayConfigRequired,
	msgGameDayEncodeSummary,
	msgVerifyFlagsRequired,
	msgVerifyEncodeResult,
	msgVerifyNoRecords,
	msgKeygenOutRequired,
	msgKeygenEncodeResult,
	msgConformanceAdapterRequired,
	msgConformanceBadSourceParam,
	msgConformanceEncodeReport,
	msgProbeNameRequired,
	msgProbeEncode,
	msgVersionProtocol,
	msgVersionSchema,
	msgUsage,
}
