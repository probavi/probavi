package spec_test

import "testing"

// The backup manifest is the one published schema written by software
// this repository does not ship: a backup job produces it, and the core
// only reads it. Nothing here can hold it to a golden file, so the
// samples below are the contract's two halves — what a backup job may
// write, and what it may not.
//
// docs/backup-manifest.md §11 is why both versions appear. v1 shipped
// 2026-09-25 and stays valid forever; v2 adds `baseline` and nothing
// else, so every v1 sample below must still validate after v2 exists.

const manifestSchema = "manifest/manifest.json"

var manifestSamples = []struct {
	name string
	doc  string
}{
	{"v1 with both expectations",
		`{"schema":"probavi-manifest/1",
			"expected_checksum":"sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
			"expected_size_bytes":4182016,
			"created_at":"2026-09-25T02:14:07.000Z","engine_version":"16.4"}`},
	{"v1 with a checksum alone",
		`{"schema":"probavi-manifest/1",
			"expected_checksum":"sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"}`},
	{"v1 with a size alone",
		`{"schema":"probavi-manifest/1","expected_size_bytes":0}`},
	{"v2 without a baseline is a v1 manifest renumbered",
		`{"schema":"probavi-manifest/2","expected_size_bytes":4182016}`},
	{"v2 with exact counts",
		`{"schema":"probavi-manifest/2","expected_size_bytes":4182016,
			"baseline":{"orders":{"rows":100000},"public.invoices":{"rows":42}}}`},
	{"v2 with a range",
		`{"schema":"probavi-manifest/2","expected_size_bytes":4182016,
			"baseline":{"customers":{"rows_min":4980,"rows_max":5020}}}`},
	{"v2 mixing an exact count and a range",
		`{"schema":"probavi-manifest/2","expected_size_bytes":4182016,
			"baseline":{"orders":{"rows":100000},"customers":{"rows_min":4980,"rows_max":5020}}}`},
	{"v2 with a zero-row table",
		`{"schema":"probavi-manifest/2","expected_size_bytes":4182016,
			"baseline":{"audit_log":{"rows":0}}}`},
}

// TestBackupManifestSamplesValidate is the positive half: every shape
// docs/backup-manifest.md invites a backup job to write is accepted.
func TestBackupManifestSamplesValidate(t *testing.T) {
	c, _ := newCompiler(t)
	manifest := compile(t, c, manifestSchema)
	for _, tc := range manifestSamples {
		t.Run(tc.name, func(t *testing.T) {
			if err := manifest.Validate(parseJSON(t, []byte(tc.doc))); err != nil {
				t.Errorf("valid backup manifest rejected: %v", err)
			}
		})
	}
}

var manifestViolations = []struct {
	name string
	doc  string
}{
	{"no schema",
		`{"expected_size_bytes":1}`},
	{"unknown schema version",
		`{"schema":"probavi-manifest/3","expected_size_bytes":1}`},
	{"asserting nothing",
		`{"schema":"probavi-manifest/1","created_at":"2026-09-25T02:14:07.000Z"}`},
	{"unknown top-level field",
		`{"schema":"probavi-manifest/1","expected_size_bytes":1,"expected_rows":5}`},
	{"checksum without the sha256 prefix",
		`{"schema":"probavi-manifest/1",
			"expected_checksum":"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"}`},
	{"checksum in uppercase hex",
		`{"schema":"probavi-manifest/1",
			"expected_checksum":"sha256:9F86D081884C7D659A2FEAA0C55AD015A3BF4F1B2B0B822CD15D6C15B0F00A08"}`},
	{"negative size",
		`{"schema":"probavi-manifest/1","expected_size_bytes":-1}`},
	{"fractional size",
		`{"schema":"probavi-manifest/1","expected_size_bytes":4182016.5}`},
	{"created_at that is not a timestamp",
		`{"schema":"probavi-manifest/1","expected_size_bytes":1,"created_at":"last night"}`},
	// The rest are v2's own: a baseline the core could read but not act
	// on honestly is worse than none, so the schema refuses each shape
	// §2.1 declines to give a meaning to.
	{"baseline on a v1 manifest",
		`{"schema":"probavi-manifest/1","expected_size_bytes":1,"baseline":{"orders":{"rows":1}}}`},
	{"empty baseline",
		`{"schema":"probavi-manifest/2","expected_size_bytes":1,"baseline":{}}`},
	{"baseline standing alone, with no checksum or size",
		`{"schema":"probavi-manifest/2","baseline":{"orders":{"rows":1}}}`},
	{"an exact count and a range together",
		`{"schema":"probavi-manifest/2","expected_size_bytes":1,
			"baseline":{"orders":{"rows":100000,"rows_min":99000,"rows_max":101000}}}`},
	{"a one-sided bound",
		`{"schema":"probavi-manifest/2","expected_size_bytes":1,"baseline":{"orders":{"rows_min":99000}}}`},
	{"an empty expectation for a table",
		`{"schema":"probavi-manifest/2","expected_size_bytes":1,"baseline":{"orders":{}}}`},
	{"a count that is not a number",
		`{"schema":"probavi-manifest/2","expected_size_bytes":1,"baseline":{"orders":{"rows":"100000"}}}`},
	{"a fractional count",
		`{"schema":"probavi-manifest/2","expected_size_bytes":1,"baseline":{"orders":{"rows":100000.5}}}`},
	{"a negative count",
		`{"schema":"probavi-manifest/2","expected_size_bytes":1,"baseline":{"orders":{"rows":-1}}}`},
	{"an unknown member beside a count",
		`{"schema":"probavi-manifest/2","expected_size_bytes":1,
			"baseline":{"orders":{"rows":1,"tolerance":"1%"}}}`},
	// A key outside the identifier rule (drill-config.md §3.5) names a
	// table no check could ever run, so it is refused where it is written
	// rather than where the reconciliation would have failed.
	{"a table name that is not an identifier",
		`{"schema":"probavi-manifest/2","expected_size_bytes":1,"baseline":{"order items":{"rows":1}}}`},
	{"a table name starting with a digit",
		`{"schema":"probavi-manifest/2","expected_size_bytes":1,"baseline":{"2024_orders":{"rows":1}}}`},
	{"a three-part qualified name",
		`{"schema":"probavi-manifest/2","expected_size_bytes":1,"baseline":{"db.public.orders":{"rows":1}}}`},
	{"an empty table name",
		`{"schema":"probavi-manifest/2","expected_size_bytes":1,"baseline":{"":{"rows":1}}}`},
	{"a baseline that is not an object",
		`{"schema":"probavi-manifest/2","expected_size_bytes":1,"baseline":[{"orders":{"rows":1}}]}`},
}

// TestBackupManifestViolations is the negative half. It matters more here
// than for the wire schemas: a malformed backup manifest is written by
// software nobody in this repository controls, and a schema that accepted
// anything would let it through to a reader that then has to guess.
func TestBackupManifestViolations(t *testing.T) {
	c, _ := newCompiler(t)
	manifest := compile(t, c, manifestSchema)
	for _, tc := range manifestViolations {
		t.Run(tc.name, func(t *testing.T) {
			if err := manifest.Validate(parseJSON(t, []byte(tc.doc))); err == nil {
				t.Error("malformed backup manifest validates, want rejection")
			}
		})
	}
}
