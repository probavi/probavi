package config

// messages.go declares this package's translatable validation
// diagnostics (docs/i18n.md §4). Constant values double as catalog
// keys — the key is the English text itself — and Messages feeds the
// per-locale gates in the cmd test suite. Config-key locators like
// "checks[%d]" and wrapper lines that only carry a nested error stay
// untranslated by design; messages produced inside YAML unmarshaling
// (durations, scalars, syntax errors) remain English until they can
// reach a translator.
const (
	msgReadConfig    = "read config: %w"
	msgConfigEmpty   = "config %s is empty"
	msgParseConfig   = "parse config %s:\n%s"
	msgInvalidConfig = "invalid config %s:\n%w"

	msgTargetNameRequired   = "target.name is required — it identifies the drill in evidence records"
	msgAdapterRequired      = `target.adapter is required (e.g. "postgres")`
	msgAdapterNamePattern   = "target.adapter %q must be lowercase letters, digits, and hyphens (it resolves to the executable probavi-adapter-%s)"
	msgSourceKindRequired   = `target.source.kind is required (adapter-defined, e.g. "pgdump" — see the adapter's probe output)`
	msgCredentialEnvName    = "target.source.credential_env entry %q is not a valid environment variable name"
	msgSourceSelectValue    = "target.source.select %q is not a selection policy (supported: %s)"
	msgSourceSelectTwice    = "target.source.select and target.source.params.select both say which backup to restore — remove one"
	msgPITRExactlyOne       = `target.pitr requires exactly one of target_time (RFC 3339, e.g. "2026-07-30T14:32:00Z") or target_age (e.g. "24h")`
	msgPITRBadTargetTime    = "target.pitr.target_time %q is not an RFC 3339 timestamp"
	msgPITRFutureTargetTime = "target.pitr.target_time %q is in the future — a restore can only be proven to an instant that has already passed"
	msgProviderRequired     = `sandbox.provider is required (e.g. "docker")`
	msgTimeoutRequired      = `sandbox.timeout is required — every drill needs a hard wall-clock limit (e.g. "30m")`
	msgChecksRequired       = `at least one check is required (start with "- builtin: service_healthy")`
	msgEvidencePathRequired = "evidence.path is required — a drill that leaves no evidence record proves nothing"
	msgSignKeyRequired      = `evidence.sign_key is required (generate one with "probavi evidence keygen")`
	msgMetricsTextfile      = "metrics.prometheus_textfile is required when the metrics section is present"

	msgCheckBuiltinOrSQLNotBoth = "%s: exactly one of builtin or sql must be set, not both"
	msgCheckBuiltinOrSQL        = "%s: exactly one of builtin or sql must be set"
	msgCheckExpectOnlySQL       = "%s: expect is only valid for sql checks"
	msgCheckNameOnlySQL         = "%s: name is only valid for sql checks (builtin checks are named automatically)"
	msgCheckUnknownBuiltin      = "%s: unknown builtin %q (supported: service_healthy, table_exists, row_count, freshness)"
	msgCheckRowCountBounds      = "%s: row_count requires min, max, or both"
	msgCheckRowCountNegative    = "%s: row_count bounds must not be negative"
	msgCheckRowCountMinMax      = "%s: row_count min (%d) exceeds max (%d)"
	msgCheckFreshnessColumn     = "%s: freshness requires column (the timestamp column to inspect)"
	msgCheckFreshnessMaxAge     = `%s: freshness requires max_age (e.g. "24h")`
	msgCheckSQLExpect           = "%s: sql checks require expect — the exact value the query must return"
	msgCheckRequiresTable       = "%s: %s requires table"
	msgCheckTableNotValid       = "%s: table is not valid for %s"
	msgCheckColumnNotValid      = "%s: column is not valid for %s"
	msgCheckMinMaxNotValid      = "%s: min/max are not valid for %s"
	msgCheckMaxAgeNotValid      = "%s: max_age is not valid for %s"

	msgNotifyWebhooksRequired  = "notify.webhooks must list at least one webhook when the notify section is present"
	msgWebhookURLNotBoth       = "%s: exactly one of url or url_env must be set, not both"
	msgWebhookURLNeither       = "%s: exactly one of url or url_env must be set (token-bearing URLs belong in url_env)"
	msgWebhookURLEnvName       = "%s: url_env %q is not a valid environment variable name"
	msgWebhookURLShape         = "%s: url must be an absolute http(s) URL"
	msgWebhookSecretEnvName    = "%s: secret_env %q is not a valid environment variable name" //nolint:gosec // G101 false positive: a diagnostic about the secret_env config key, not a credential
	msgWebhookUnknownOutcome   = "%s: unknown outcome %q in on (supported: pass, fail, error, cancelled)"
	msgWebhookDuplicateOutcome = "%s: duplicate outcome %q in on"

	msgReadGameDay    = "read game-day config: %w"
	msgGameDayEmpty   = "game-day config %s is empty"
	msgParseGameDay   = "parse game-day config %s:\n%s"
	msgInvalidGameDay = "invalid game-day config %s:\n%w"

	msgGameDayNameRequired    = "name is required — it identifies the exercise in the summary"
	msgGameDayTimeoutRequired = `timeout is required — every game-day needs a hard wall-clock limit (e.g. "2h")`
	msgMaxParallelNegative    = "max_parallel must not be negative"
	msgMembersRequired        = "at least one member is required"
	msgMemberNameRequired     = "members[%d]: name is required"
	msgMemberNameDuplicate    = "members[%d]: name %q duplicates members[%d]"
	msgMemberConfigRequired   = "members[%d]: config is required — the member's drill configuration file"
	msgMemberSelfDependency   = "members[%d]: depends_on must not reference the member itself"
	msgMemberDuplicateDep     = "members[%d]: duplicate dependency %q"
	msgMemberUnknownDep       = "members[%d]: depends_on references unknown member %q"
	msgDependencyCycle        = "dependency cycle involving members: %s"
	msgSharedEvidenceLog      = "members %s and %s share evidence log %s while max_parallel is %d — concurrent drills against one log fail on its single-writer lock; use per-member logs or max_parallel: 1"
)

// Messages is this package's complete translatable surface; the cmd
// i18n gates iterate it together with the CLI message set.
func Messages() []string {
	return []string{
		msgReadConfig,
		msgConfigEmpty,
		msgParseConfig,
		msgInvalidConfig,
		msgTargetNameRequired,
		msgAdapterRequired,
		msgAdapterNamePattern,
		msgSourceKindRequired,
		msgCredentialEnvName,
		msgSourceSelectValue,
		msgSourceSelectTwice,
		msgPITRExactlyOne,
		msgPITRBadTargetTime,
		msgPITRFutureTargetTime,
		msgProviderRequired,
		msgTimeoutRequired,
		msgChecksRequired,
		msgEvidencePathRequired,
		msgSignKeyRequired,
		msgMetricsTextfile,
		msgCheckBuiltinOrSQLNotBoth,
		msgCheckBuiltinOrSQL,
		msgCheckExpectOnlySQL,
		msgCheckNameOnlySQL,
		msgCheckUnknownBuiltin,
		msgCheckRowCountBounds,
		msgCheckRowCountNegative,
		msgCheckRowCountMinMax,
		msgCheckFreshnessColumn,
		msgCheckFreshnessMaxAge,
		msgCheckSQLExpect,
		msgCheckRequiresTable,
		msgCheckTableNotValid,
		msgCheckColumnNotValid,
		msgCheckMinMaxNotValid,
		msgCheckMaxAgeNotValid,
		msgNotifyWebhooksRequired,
		msgWebhookURLNotBoth,
		msgWebhookURLNeither,
		msgWebhookURLEnvName,
		msgWebhookURLShape,
		msgWebhookSecretEnvName,
		msgWebhookUnknownOutcome,
		msgWebhookDuplicateOutcome,
		msgReadGameDay,
		msgGameDayEmpty,
		msgParseGameDay,
		msgInvalidGameDay,
		msgGameDayNameRequired,
		msgGameDayTimeoutRequired,
		msgMaxParallelNegative,
		msgMembersRequired,
		msgMemberNameRequired,
		msgMemberNameDuplicate,
		msgMemberConfigRequired,
		msgMemberSelfDependency,
		msgMemberDuplicateDep,
		msgMemberUnknownDep,
		msgDependencyCycle,
		msgSharedEvidenceLog,
	}
}
