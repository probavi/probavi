package config

// builtins.go is the registry of check kinds: the four built-ins plus the
// user-defined SQL assertion. It is the single gate for the vocabulary —
// validate() resolves every configured check through it, so a kind absent
// here cannot be configured and therefore cannot reach internal/checks —
// and it is what lets the generated capabilities manifest
// (docs/capabilities.md) enumerate the checks without a second list.
//
// The per-kind field rules stay in config.go next to their diagnostics;
// this file owns the vocabulary, not the rules.

// Check kind identifiers.
const (
	CheckServiceHealthy = "service_healthy"
	CheckTableExists    = "table_exists"
	CheckRowCount       = "row_count"
	CheckFreshness      = "freshness"
	// CheckSQL is the user-defined assertion, configured with sql/expect
	// rather than builtin — hence Builtin false in the registry.
	CheckSQL = "sql"
)

// Parameter type names used by CheckParam.Type. They describe the YAML
// value a drill author writes, not a Go type.
const (
	ParamIdentifier = "identifier"
	ParamInteger    = "integer"
	ParamDuration   = "duration"
	ParamString     = "string"
	ParamScalar     = "scalar"
)

// CheckParam is one configurable parameter of a check kind.
type CheckParam struct {
	// Name is the YAML key.
	Name string
	// Type is one of the Param* constants.
	Type string
	// Required reports whether omitting the key is a validation error.
	Required bool
	// Doc is a one-line English description.
	Doc string
}

// CheckKind is one runnable validation shape.
type CheckKind struct {
	// ID is the YAML value of builtin, or "sql" for the SQL assertion.
	ID string
	// Name is a one-line English label.
	Name string
	// Status is the maturity level, validated against the capabilities
	// vocabulary (docs/capabilities.md).
	Status string
	// Builtin reports whether the kind is selected with the builtin key.
	Builtin bool
	// Params are the kind's parameters, in documentation order.
	Params []CheckParam
	// Requires states a rule the parameter list alone cannot express, ""
	// when there is none.
	Requires string
}

// CheckKinds returns the registry in documentation order. It returns a
// fresh slice on every call: the registry is a contract, not shared state.
func CheckKinds() []CheckKind {
	return []CheckKind{
		{
			ID:      CheckServiceHealthy,
			Status:  "experimental",
			Name:    "Engine answers the adapter's healthcheck",
			Builtin: true,
		},
		{
			ID:      CheckTableExists,
			Status:  "experimental",
			Name:    "Table exists and is queryable",
			Builtin: true,
			Params: []CheckParam{
				{Name: "table", Type: ParamIdentifier, Required: true, Doc: "What the check is about: a table, collection, class, label or key prefix, depending on the engine. The adapter decides what it names, and its README says which."},
			},
		},
		{
			ID:      CheckRowCount,
			Status:  "experimental",
			Name:    "Row count within bounds",
			Builtin: true,
			Params: []CheckParam{
				{Name: "table", Type: ParamIdentifier, Required: true, Doc: "What the check is about: a table, collection, class, label or key prefix, depending on the engine. The adapter decides what it names, and its README says which."},
				{Name: "min", Type: ParamInteger, Doc: "Lower bound, inclusive."},
				{Name: "max", Type: ParamInteger, Doc: "Upper bound, inclusive."},
			},
			Requires: "At least one of min, max.",
		},
		{
			ID:      CheckFreshness,
			Status:  "experimental",
			Name:    "Newest row younger than a maximum age",
			Builtin: true,
			Params: []CheckParam{
				{Name: "table", Type: ParamIdentifier, Required: true, Doc: "What the check is about: a table, collection, class, label or key prefix, depending on the engine. The adapter decides what it names, and its README says which."},
				{Name: "column", Type: ParamIdentifier, Required: true, Doc: "The dated field to take the maximum of — a column, property or attribute, named the way the engine names one."},
				{Name: "max_age", Type: ParamDuration, Required: true, Doc: "Maximum age of the newest row."},
			},
		},
		{
			ID:     CheckSQL,
			Status: "experimental",
			Name:   "Custom SQL assertion",
			Params: []CheckParam{
				{Name: "sql", Type: ParamString, Required: true, Doc: "Statement run through the adapter-declared sql_runner."},
				{Name: "expect", Type: ParamScalar, Required: true, Doc: "Expected single scalar result."},
				{Name: "name", Type: ParamString, Doc: "Check name recorded in evidence; defaults to the check index."},
			},
		},
	}
}

// LookupCheckKind resolves a check kind identifier.
func LookupCheckKind(id string) (CheckKind, bool) {
	for _, k := range CheckKinds() {
		if k.ID == id {
			return k, true
		}
	}
	return CheckKind{}, false
}
