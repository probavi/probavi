// Package cli declares Probavi's command-line contract as data: the
// command tree, the flags each command takes, and the exit codes each one
// can return. cmd/probavi resolves and dispatches through this table, so a
// command absent from it cannot be invoked — which is what lets the
// generated capabilities manifest (docs/capabilities.md) state the CLI
// surface without a second, hand-maintained list.
//
// The strings here are machine contract, not terminal prose: the
// translated usage and diagnostic text lives in cmd/probavi and is never
// derived from this package (docs/i18n.md §1).
package cli

// Exit codes — the cron/CI contract shared by every command. Which of
// them a given command can return, and what each one means for it, is
// declared per command below; verify verdicts follow evidence-schema.md
// §9.
const (
	// ExitPass reports success: for a drill, a backup proven restorable.
	ExitPass = 0
	// ExitFail reports a recoverability failure or a failed check — the
	// command worked, the answer is no.
	ExitFail = 1
	// ExitError reports an infrastructure error or a cancelled run: the
	// question could not be answered.
	ExitError = 2
	// ExitUsage reports a usage, setup, or I/O error, distinct from any
	// verdict.
	ExitUsage = 3
	// ExitEvidenceLost reports that a drill ran but its evidence record
	// could not be written — the most severe outcome a drill has.
	ExitEvidenceLost = 5
)

// Command identifiers. They are the dispatch keys in cmd/probavi and the
// command ids of the generated capabilities manifest; a command word path
// joined by spaces.
const (
	CmdRun                = "run"
	CmdGameDay            = "gameday"
	CmdPush               = "push"
	CmdEvidenceVerify     = "evidence verify"
	CmdEvidenceKeygen     = "evidence keygen"
	CmdAdapterProbe       = "adapter probe"
	CmdAdapterConformance = "adapter conformance"
	CmdVersion            = "version"
)

// Flag is one command-line flag of a command.
type Flag struct {
	// Name is the flag as typed, including the leading dashes.
	Name string
	// Required reports whether omitting the flag is a usage error.
	Required bool
	// Repeatable reports whether the flag may be given more than once.
	Repeatable bool
	// Doc is a one-line English description.
	Doc string
}

// ExitCode is one exit status a command can return, with the meaning that
// command attaches to it.
type ExitCode struct {
	Code    int
	Meaning string
}

// Command is one invocable command of the probavi binary.
type Command struct {
	// ID is the command word path joined by spaces ("evidence verify").
	ID string
	// Words is the command path as typed.
	Words []string
	// Status is the maturity level, validated against the capabilities
	// vocabulary (docs/capabilities.md).
	Status string
	// Summary is a one-line English description.
	Summary string
	// Flags are the command's flags, in usage order.
	Flags []Flag
	// Positional describes the positional argument, "" when there is none.
	Positional string
	// Stdout describes what the command writes to stdout, "" when it
	// writes nothing there.
	Stdout string
	// ExitCodes are every status the command can return, ascending.
	ExitCodes []ExitCode
	// Docs is a repository-relative path to the normative document for
	// this command, "" when none applies.
	Docs string
}

// Commands returns the full command table, in usage order. It returns a
// fresh slice on every call: the table is a contract, not shared state.
func Commands() []Command {
	return []Command{
		{
			ID:      CmdRun,
			Words:   []string{"run"},
			Status:  "experimental",
			Summary: "Execute one restore drill: sandbox up, restore, checks, teardown, and exactly one signed evidence record.",
			Flags: []Flag{
				{Name: "--config", Required: true, Doc: "Path to the drill configuration YAML."},
			},
			Stdout: "one-line JSON drill summary",
			ExitCodes: []ExitCode{
				{Code: ExitPass, Meaning: "backup proven restorable"},
				{Code: ExitFail, Meaning: "recoverability failure (backup, restore, or check)"},
				{Code: ExitError, Meaning: "infrastructure error or cancelled"},
				{Code: ExitUsage, Meaning: "usage or setup error"},
				{Code: ExitEvidenceLost, Meaning: "evidence record could not be written"},
			},
		},
		{
			ID:      CmdGameDay,
			Words:   []string{"gameday"},
			Status:  "experimental",
			Summary: "Execute a DR game-day: member drills in dependency order, each leaving its own signed evidence record.",
			Flags: []Flag{
				{Name: "--config", Required: true, Doc: "Path to the game-day configuration YAML."},
			},
			Stdout: "one-line JSON game-day summary",
			ExitCodes: []ExitCode{
				{Code: ExitPass, Meaning: "every member passed"},
				{Code: ExitFail, Meaning: "a member drill failed"},
				{Code: ExitError, Meaning: "errors or cancellation left members unproven"},
				{Code: ExitUsage, Meaning: "usage or setup error"},
				{Code: ExitEvidenceLost, Meaning: "a member's evidence record could not be written"},
			},
			Docs: "docs/gameday.md",
		},
		{
			ID:      CmdPush,
			Words:   []string{"push"},
			Status:  "experimental",
			Summary: "Send an evidence log to a URL: the whole file, unchanged, as application/x-ndjson. A copy — the log is never modified.",
			Flags: []Flag{
				{Name: "--log", Required: true, Doc: "Path to the evidence log to send; opened read-only."},
				{Name: "--to", Doc: "Destination as a literal absolute http(s) URL; exactly one of --to or --to-env."},
				{Name: "--to-env", Doc: "Environment variable holding the destination URL; use this when the URL carries a token."},
				{Name: "--path", Doc: "Path the log is sent under, appended to the destination URL; defaults to the log file's base name."},
				{Name: "--token-env", Doc: "Environment variable holding the bearer token; defaults to PROBAVI_PUSH_TOKEN."},
				{Name: "--allow-unauthenticated", Doc: "Send no Authorization header; mutually exclusive with --token-env."},
				{Name: "--secret-env", Doc: "Environment variable holding the HMAC secret for body signing; absent means unsigned."},
			},
			Stdout: "one-line JSON push result",
			ExitCodes: []ExitCode{
				{Code: ExitPass, Meaning: "the log was accepted"},
				{Code: ExitError, Meaning: "delivery failure: attempts exhausted or the receiver refused"},
				{Code: ExitUsage, Meaning: "usage, configuration, or I/O error"},
			},
			Docs: "docs/evidence-push.md",
		},
		{
			ID:      CmdEvidenceVerify,
			Words:   []string{"evidence", "verify"},
			Status:  "experimental",
			Summary: "Verify an evidence log offline against one or more public keys.",
			Flags: []Flag{
				{Name: "--log", Required: true, Doc: "Path to the evidence log file."},
				{Name: "--key", Required: true, Repeatable: true, Doc: "Public key file; repeat to build a keyring."},
			},
			Stdout: "one-line JSON verification result",
			ExitCodes: []ExitCode{
				{Code: ExitPass, Meaning: "VALID"},
				{Code: ExitFail, Meaning: "VALID_WITH_DAMAGE"},
				{Code: ExitError, Meaning: "INVALID"},
				{Code: ExitUsage, Meaning: "usage or I/O error"},
			},
			Docs: "docs/evidence-schema.md",
		},
		{
			ID:      CmdEvidenceKeygen,
			Words:   []string{"evidence", "keygen"},
			Status:  "experimental",
			Summary: "Generate an ed25519 signing key pair; refuses to overwrite existing files.",
			Flags: []Flag{
				{Name: "--out", Required: true, Doc: "Path for the private key; the public key is written next to it as <path>.pub."},
			},
			Stdout: "one-line JSON key identity, never key material",
			ExitCodes: []ExitCode{
				{Code: ExitPass, Meaning: "key pair written"},
				{Code: ExitUsage, Meaning: "usage or I/O error"},
			},
			Docs: "docs/evidence-schema.md",
		},
		{
			ID:         CmdAdapterProbe,
			Words:      []string{"adapter", "probe"},
			Status:     "experimental",
			Summary:    "Resolve probavi-adapter-<name> and print its declared capabilities.",
			Positional: "<name>",
			Stdout:     "JSON probe response",
			ExitCodes: []ExitCode{
				{Code: ExitPass, Meaning: "probe succeeded"},
				{Code: ExitError, Meaning: "the adapter failed to probe"},
				{Code: ExitUsage, Meaning: "usage error or the adapter could not be resolved"},
			},
			Docs: "docs/adapter-protocol.md",
		},
		{
			ID:      CmdAdapterConformance,
			Words:   []string{"adapter", "conformance"},
			Status:  "experimental",
			Summary: "Drive an adapter through the frozen protocol conformance checks against a simulated sandbox — no container runtime involved.",
			Flags: []Flag{
				{Name: "--source-kind", Doc: "Source kind for the provision checks; defaults to the first kind the probe declares."},
				{Name: "--source-param", Repeatable: true, Doc: "Source parameter as k=v for the provision checks."},
			},
			Positional: "<name-or-path>",
			Stdout:     "JSON conformance report",
			ExitCodes: []ExitCode{
				{Code: ExitPass, Meaning: "conformant"},
				{Code: ExitFail, Meaning: "one or more checks failed"},
				{Code: ExitError, Meaning: "the suite could not be run to completion"},
				{Code: ExitUsage, Meaning: "usage error or the adapter could not be resolved"},
			},
			Docs: "docs/adapter-protocol.md",
		},
		{
			ID:      CmdVersion,
			Words:   []string{"version"},
			Status:  "experimental",
			Summary: "Print the binary version and the contract versions this build speaks.",
			Stdout:  "version lines",
			ExitCodes: []ExitCode{
				{Code: ExitPass, Meaning: "version printed"},
			},
		},
	}
}

// Groups lists the command words that only exist as prefixes of
// subcommands ("adapter", "evidence"), in table order.
func Groups() []string {
	table := Commands()
	groups := make([]string, 0, len(table))
	seen := map[string]bool{}
	for _, c := range table {
		if len(c.Words) < 2 || seen[c.Words[0]] {
			continue
		}
		seen[c.Words[0]] = true
		groups = append(groups, c.Words[0])
	}
	return groups
}

// Resolution reports how Resolve read an argument list.
type Resolution int

const (
	// ResolvedCommand means Match.Command and Match.Args are usable.
	ResolvedCommand Resolution = iota
	// UnknownCommand means the first word names no command and no group.
	UnknownCommand
	// IncompleteGroup means the first word names a group with nothing
	// after it.
	IncompleteGroup
	// UnknownSubcommand means the first word names a group and the second
	// names no subcommand of it.
	UnknownSubcommand
)

// Match is the outcome of resolving an argument list against the table.
type Match struct {
	// Resolution says which of the other fields are meaningful.
	Resolution Resolution
	// Command is the resolved command (ResolvedCommand only).
	Command Command
	// Args are the arguments left after the command words.
	Args []string
	// Group is the group word (IncompleteGroup, UnknownSubcommand).
	Group string
	// Word is the argument that could not be resolved (UnknownCommand,
	// UnknownSubcommand).
	Word string
}

// Resolve maps an argument list onto the command table. It never reports
// a partial match as success: a caller that receives anything other than
// ResolvedCommand must print usage rather than guess.
func Resolve(args []string) Match {
	if len(args) == 0 {
		return Match{Resolution: UnknownCommand}
	}
	table := Commands()
	for _, c := range table {
		if len(c.Words) == 1 && c.Words[0] == args[0] {
			return Match{Command: c, Args: args[1:]}
		}
	}
	if !isGroup(table, args[0]) {
		return Match{Resolution: UnknownCommand, Word: args[0]}
	}
	if len(args) == 1 {
		return Match{Resolution: IncompleteGroup, Group: args[0]}
	}
	for _, c := range table {
		if len(c.Words) == 2 && c.Words[0] == args[0] && c.Words[1] == args[1] {
			return Match{Command: c, Args: args[2:]}
		}
	}
	return Match{Resolution: UnknownSubcommand, Group: args[0], Word: args[1]}
}

func isGroup(table []Command, word string) bool {
	for _, c := range table {
		if len(c.Words) > 1 && c.Words[0] == word {
			return true
		}
	}
	return false
}
