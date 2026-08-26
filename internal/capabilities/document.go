package capabilities

// document.go declares the shape of docs/capabilities.json. Struct field
// order is the serialization order — deterministic and readable, which a
// map is not — and every field is always present: an absent optional
// string renders as null, never omitted, so a consumer never has to tell
// "missing" from "unknown" (docs/capabilities.md).

// Document is the whole of docs/capabilities.json.
type Document struct {
	Schema           string            `json:"$schema"`
	Generated        string            `json:"_generated"`
	SchemaID         string            `json:"schema"`
	Project          Project           `json:"project"`
	Contracts        Contracts         `json:"contracts"`
	Adapters         []Adapter         `json:"adapters"`
	SandboxProviders []SandboxProvider `json:"sandbox_providers"`
	Checks           []Check           `json:"checks"`
	CLI              CLI               `json:"cli"`
	Notifications    Notifications     `json:"notifications"`
	Locales          Locales           `json:"locales"`
	NonGoals         []NonGoal         `json:"non_goals"`
}

// Project states repository-level facts.
type Project struct {
	Status     string `json:"status"`
	License    string `json:"license"`
	Repository string `json:"repository"`
}

// Contracts states the versions of the four independently versioned
// contracts this build speaks.
type Contracts struct {
	AdapterProtocol     Contract         `json:"adapter_protocol"`
	EvidenceSchema      EvidenceContract `json:"evidence_schema"`
	NotificationPayload Contract         `json:"notification_payload"`
	EvidencePush        Contract         `json:"evidence_push"`
}

// Contract is one versioned contract with its normative document.
type Contract struct {
	Version string `json:"version"`
	Spec    string `json:"spec"`
	Schema  string `json:"schema"`
}

// EvidenceContract additionally reports every schema version the verifier
// accepts and where the independent verifier lives.
type EvidenceContract struct {
	Version             string   `json:"version"`
	ReadableVersions    []string `json:"readable_versions"`
	Spec                string   `json:"spec"`
	Schema              string   `json:"schema"`
	IndependentVerifier string   `json:"independent_verifier"`
}

// Adapter is one engine adapter shipped in this repository.
type Adapter struct {
	ID                  string           `json:"id"`
	Name                string           `json:"name"`
	Status              string           `json:"status"`
	Since               *string          `json:"since"`
	AdapterVersion      string           `json:"adapter_version"`
	Engine              Engine           `json:"engine"`
	ProtocolVersions    []string         `json:"protocol_versions"`
	Verified            []VerifiedTarget `json:"verified"`
	Sources             []Source         `json:"sources"`
	PITR                bool             `json:"pitr"`
	ConformanceVerified bool             `json:"conformance_verified"`
	Docs                *string          `json:"docs"`
}

// Engine identifies the database engine an adapter restores.
type Engine struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// VerifiedTarget is an engine version this repository's integration suite
// actually restores from. It is a record of what was tested, never a
// claim of supported version ranges (docs/capabilities.md).
type VerifiedTarget struct {
	EngineVersion string `json:"engine_version"`
	Image         string `json:"image"`
}

// Source is one backup source kind an adapter accepts.
type Source struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	PITR bool   `json:"pitr"`
}

// SandboxProvider is one disposable-runtime provider.
type SandboxProvider struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	Status          string          `json:"status"`
	Params          []ProviderParam `json:"params"`
	Isolation       Isolation       `json:"isolation"`
	Constraints     []string        `json:"constraints"`
	VerifiedAgainst []string        `json:"verified_against"`
	Docs            *string         `json:"docs"`
}

// ProviderParam is one drill-config sandbox parameter.
type ProviderParam struct {
	ID       string  `json:"id"`
	Required bool    `json:"required"`
	Default  *string `json:"default"`
	Doc      string  `json:"doc"`
}

// Isolation states a provider's containment properties.
type Isolation struct {
	NetworkDefault   *string `json:"network_default"`
	PublishedPorts   bool    `json:"published_ports"`
	Storage          string  `json:"storage"`
	ForcedTeardown   bool    `json:"forced_teardown"`
	OrphanSweep      string  `json:"orphan_sweep"`
	ExternalBackstop *string `json:"external_backstop"`
}

// Check is one validation a drill can run against a restored database.
type Check struct {
	ID       string       `json:"id"`
	Name     string       `json:"name"`
	Status   string       `json:"status"`
	Kind     string       `json:"kind"`
	Params   []CheckParam `json:"params"`
	Requires *string      `json:"requires"`
}

// Check kinds: how a check is selected in drill config.
const (
	// CheckKindBuiltin is selected with the builtin key.
	CheckKindBuiltin = "builtin"
	// CheckKindSQL is the user-defined assertion.
	CheckKindSQL = "sql"
)

// CheckParam is one configurable parameter of a check.
type CheckParam struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
	Doc      string `json:"doc"`
}

// CLI states the command-line surface.
type CLI struct {
	Binary   string    `json:"binary"`
	Commands []Command `json:"commands"`
}

// Command is one invocable command with its exit-code contract.
type Command struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Status     string     `json:"status"`
	Summary    string     `json:"summary"`
	Flags      []Flag     `json:"flags"`
	Positional *string    `json:"positional"`
	Stdout     *string    `json:"stdout"`
	ExitCodes  []ExitCode `json:"exit_codes"`
	Docs       *string    `json:"docs"`
}

// Flag is one command-line flag.
type Flag struct {
	ID         string `json:"id"`
	Required   bool   `json:"required"`
	Repeatable bool   `json:"repeatable"`
	Doc        string `json:"doc"`
}

// ExitCode is one exit status with the meaning its command attaches to it.
type ExitCode struct {
	Code    int    `json:"code"`
	Meaning string `json:"meaning"`
}

// Notifications states the drill-completion notification surface.
type Notifications struct {
	PayloadVersion string      `json:"payload_version"`
	Event          string      `json:"event"`
	Transports     []Transport `json:"transports"`
}

// Transport is one delivery mechanism.
type Transport struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Status        string   `json:"status"`
	Method        string   `json:"method"`
	ContentType   string   `json:"content_type"`
	EventHeader   string   `json:"event_header"`
	Signing       Signing  `json:"signing"`
	OutcomeFilter []string `json:"outcome_filter"`
	Delivery      Delivery `json:"delivery"`
	Docs          *string  `json:"docs"`
}

// Signing states how a receiver authenticates a delivery.
type Signing struct {
	Algorithm string `json:"algorithm"`
	Header    string `json:"header"`
	Optional  bool   `json:"optional"`
}

// Delivery states the fixed retry budget.
type Delivery struct {
	Attempts              int `json:"attempts"`
	AttemptTimeoutSeconds int `json:"attempt_timeout_seconds"`
	TotalBudgetSeconds    int `json:"total_budget_seconds"`
}

// Locales states which languages the CLI speaks and what is translated.
type Locales struct {
	Source    string   `json:"source"`
	Available []string `json:"available"`
	Scope     string   `json:"scope"`
	Docs      *string  `json:"docs"`
}

// NonGoal is something Probavi deliberately does not do. Consumers must
// never contradict these statements.
type NonGoal struct {
	ID        string `json:"id"`
	Statement string `json:"statement"`
}

// nullable renders an absent optional string as JSON null rather than as
// an empty string, so every field of the document is always present with
// an unambiguous value.
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
