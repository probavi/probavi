// Package capabilities generates docs/capabilities.json: the
// machine-readable statement of what Probavi can do in this repository
// today. The contract for consumers is docs/capabilities.md, which is
// normative.
//
// Every fact is read from the code registry that implements the
// capability — the adapters' own probe goldens, internal/sandbox/registry,
// config.CheckKinds, internal/cli, internal/notify, internal/i18n, and the
// frozen contract version constants. What no code can hold (display names,
// maturity, the engine versions CI restores from) is declared next to the
// code it describes and cross-checked here, so this package never learns
// anything the repository does not already state. Facts that cannot exist
// in code at all — the project's own maturity and the non-goals — are
// declared in this package, once, and nowhere else.
//
// The rendered bytes are deterministic and carry no volatile field: no
// timestamp, no build metadata. The file changes when a capability
// changes, and CI fails on any difference (AGENTS.md §5.8).
package capabilities

//go:generate go run ../tools/capabilities -root ../.. -out ../../docs/capabilities.json

// The README's engine table is generated from the manifest the directive
// above writes, so the two run in this order, in this file, on purpose.
//go:generate go run ../tools/enginetable -root ../..

const (
	// SchemaID is this document's format identifier. It is versioned
	// independently of the binary, exactly like probavi-adapter/N,
	// probavi-evidence/N, and probavi-notification/N: within a version
	// fields may be added and entries may come and go; removing or
	// renaming a field, or changing its meaning, needs a new version.
	SchemaID = "probavi-capabilities/1"

	// SchemaURL is the $schema of the rendered document.
	SchemaURL = "https://probavi.dev/schemas/capabilities/capabilities.json"

	// Path is the repository-relative output path. It is fixed: downstream
	// consumers read this repository as a submodule and point at it.
	Path = "docs/capabilities.json"

	// SchemaPath is the repository-relative path of the JSON Schema.
	SchemaPath = "docs/schemas/capabilities/capabilities.json"

	// ContractDoc is the repository-relative path of the consumer
	// contract.
	ContractDoc = "docs/capabilities.md"

	// GeneratedMarker is the in-file notice. JSON has no comments, so a
	// key is the only marker that survives every editor and diff tool; it
	// sorts to the top of the document where a human opening the file sees
	// it first.
	GeneratedMarker = "GENERATED FILE — do not edit by hand. Produced by `go generate ./...` from the code that implements each capability; edits are overwritten and CI fails on any diff. Contract: " + ContractDoc
)

// Maturity values. Everything ships experimental while the project is
// pre-alpha; beta and stable are reserved so the vocabulary exists before
// it is earned (docs/capabilities.md).
const (
	StatusExperimental = "experimental"
	StatusBeta         = "beta"
	StatusStable       = "stable"
)

// Project-level declarations. These are the only facts in the document
// that no code can derive — they are judgements, not behavior — so they
// are declared here, once, and nowhere else.
const (
	// ProjectStatus is the project's overall maturity. Raise it only
	// alongside a release that earns it.
	ProjectStatus = "pre-alpha"
	// ProjectLicense is the SPDX identifier of LICENSE.
	ProjectLicense = "Apache-2.0"
	// ProjectRepository is the canonical source location.
	ProjectRepository = "https://github.com/probavi/probavi"
	// CLIBinary is the name of the shipped binary.
	CLIBinary = "probavi"
)

// Statuses returns the permitted maturity values.
func Statuses() []string {
	return []string{StatusExperimental, StatusBeta, StatusStable}
}

func validStatus(s string) bool {
	for _, v := range Statuses() {
		if v == s {
			return true
		}
	}
	return false
}
