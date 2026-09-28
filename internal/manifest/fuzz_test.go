package manifest_test

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/probavi/probavi/internal/manifest"
)

// FuzzCheck drives the backup-manifest check over arbitrary manifest bytes.
//
// The backup manifest is the one document in a drill that Probavi does not
// write: a backup job produces it, beside its backup, and this package
// decides from it whether the drill proceeds at all. That is the shape every
// other fuzz target in this repository has — a reader of somebody else's
// document — and this was the only such reader without one.
//
// Survival is the least of what it asserts. An expectation a manifest
// carries reaches internal/checks, where Satisfied and String dereference
// RowsMin and RowsMax without checking them, on the strength of this
// package's validation and nothing else. So every expectation that survives
// is exercised here, against the promise that makes those dereferences safe:
// exactly one form, both bounds together, neither negative, and a range that
// is one. The last of those is why a witness is computed rather than a
// constant — an expectation nothing can satisfy is a table that can never
// reconcile, which is a drill that fails for a reason the operator did not
// write.
func FuzzCheck(f *testing.F) {
	dir := f.TempDir()
	artifact := filepath.Join(dir, "backup.dump")
	const content = "the bytes a backup job wrote"
	if err := os.WriteFile(artifact, []byte(content), 0o600); err != nil {
		f.Fatalf("write artifact: %v", err)
	}
	sum := sha256.Sum256([]byte(content))
	checksum := fmt.Sprintf("sha256:%x", sum)

	for _, seed := range []string{
		// The two published versions, agreeing with the artifact.
		fmt.Sprintf(`{"schema":%q,"expected_checksum":%q}`, manifest.SchemaID, checksum),
		fmt.Sprintf(`{"schema":%q,"expected_checksum":%q}`, manifest.SchemaIDv1, checksum),
		fmt.Sprintf(`{"schema":%q,"expected_size_bytes":%d}`, manifest.SchemaID, len(content)),
		// Both baseline forms, and the shapes validation has to refuse.
		fmt.Sprintf(`{"schema":%q,"baseline":{"orders":{"rows":10}}}`, manifest.SchemaID),
		fmt.Sprintf(`{"schema":%q,"baseline":{"sales.orders":{"rows_min":1,"rows_max":9}}}`, manifest.SchemaID),
		fmt.Sprintf(`{"schema":%q,"baseline":{"orders":{"rows":1,"rows_min":1,"rows_max":2}}}`, manifest.SchemaID),
		fmt.Sprintf(`{"schema":%q,"baseline":{"orders":{"rows_min":9,"rows_max":1}}}`, manifest.SchemaID),
		fmt.Sprintf(`{"schema":%q,"baseline":{"orders":{"rows_min":1}}}`, manifest.SchemaID),
		fmt.Sprintf(`{"schema":%q,"baseline":{"orders":{}}}`, manifest.SchemaID),
		fmt.Sprintf(`{"schema":%q,"baseline":{}}`, manifest.SchemaID),
		fmt.Sprintf(`{"schema":%q,"baseline":{"1 bad name":{"rows":1}}}`, manifest.SchemaID),
		fmt.Sprintf(`{"schema":%q,"baseline":{"orders":{"rows":-1}}}`, manifest.SchemaID),
		fmt.Sprintf(`{"schema":%q,"baseline":{"orders":{"rows":9223372036854775807}}}`, manifest.SchemaID),
		// A v1 manifest is not allowed a baseline at all.
		fmt.Sprintf(`{"schema":%q,"baseline":{"orders":{"rows":1}}}`, manifest.SchemaIDv1),
		// Documents that are not manifests.
		`{"schema":"probavi-manifest/99"}`,
		`{"expected_checksum":"sha256:nothex"}`,
		`{"schema":"probavi-manifest/2","surprise":1}`,
		`{} {}`,
		`[]`,
		`null`,
		``,
		"\x00\xff",
	} {
		f.Add([]byte(seed))
	}

	path := filepath.Join(dir, "backup.manifest.json")
	f.Fuzz(func(t *testing.T, data []byte) {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Skipf("write fixture: %v", err)
		}
		res, fault := manifest.Check(artifact, path)
		assertFaultIsReportable(t, fault)
		assertVerdictAgreesWithItsFault(t, res, fault)
		assertBaselineIsUsable(t, res.Baseline)
	})
}

// assertFaultIsReportable: a fault becomes a record's error object, so it
// carries a code the schema knows and a message somebody can act on.
func assertFaultIsReportable(t *testing.T, fault *manifest.Fault) {
	t.Helper()
	if fault == nil {
		return
	}
	if fault.Code == "" || fault.Message == "" {
		t.Fatalf("fault with no code or no message: %+v", fault)
	}
	if fault.Error() != fault.Message {
		t.Fatalf("fault renders %q, want its message %q", fault.Error(), fault.Message)
	}
}

// assertVerdictAgreesWithItsFault: the record carries both halves, so they
// may never contradict each other. A hash without a manifest, a verdict
// about a manifest that was never read, or agreement beside a fault would
// each put a sentence in an append-only log that its neighbour denies.
func assertVerdictAgreesWithItsFault(t *testing.T, res manifest.Result, fault *manifest.Fault) {
	t.Helper()
	if res.Hash != nil && !strings.HasPrefix(*res.Hash, "sha256:") {
		t.Fatalf("manifest hash = %q, want a sha256: digest — it reaches a signed record", *res.Hash)
	}
	if res.Match != nil && res.Hash == nil {
		t.Fatal("a verdict about a manifest that was never read")
	}
	switch {
	case fault == nil && (res.Match == nil || !*res.Match):
		t.Fatalf("no fault, but match = %v — agreement has to be stated", res.Match)
	case fault != nil && res.Match != nil && *res.Match:
		t.Fatalf("match true beside fault %v", fault)
	}
}

// assertBaselineIsUsable: what survives here is what internal/checks
// dereferences without asking.
func assertBaselineIsUsable(t *testing.T, baseline map[string]manifest.Expectation) {
	t.Helper()
	if baseline != nil && len(baseline) == 0 {
		t.Fatal("an empty baseline survived; the schema requires at least one table")
	}
	for table, e := range baseline {
		if table == "" {
			t.Fatal("a baseline table with no name")
		}
		witness, ok := satisfiableAt(e)
		if !ok {
			t.Fatalf("expectation for %q states neither a count nor a range: %+v", table, e)
		}
		if !e.Satisfied(witness) {
			t.Fatalf("expectation for %q is satisfied by no row count, %d included: %s", table, witness, e)
		}
		if e.String() == "" {
			t.Fatalf("expectation for %q renders as nothing", table)
		}
	}
}

// satisfiableAt returns a row count the expectation must accept, and
// whether the expectation says anything at all.
func satisfiableAt(e manifest.Expectation) (int64, bool) {
	switch {
	case e.Rows != nil:
		return *e.Rows, true
	case e.RowsMin != nil:
		return *e.RowsMin, true
	default:
		return 0, false
	}
}
