package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writeFakeBinary drops a file the digest can be taken of, standing in for
// an executable.
func writeFakeBinary(t *testing.T, name, content string) (path, digest string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	sum := sha256.Sum256([]byte(content))
	return path, "sha256:" + hex.EncodeToString(sum[:])
}

// TestRecordCarriesBuildIdentity is the point of schema v2: a record says
// which adapter build performed the restore and which probavi build signed
// the result, not merely their self-reported version numbers
// (evidence-schema.md §3).
func TestRecordCarriesBuildIdentity(t *testing.T) {
	adapterPath, adapterDigest := writeFakeBinary(t, "probavi-adapter-postgres", "adapter bytes")
	corePath, coreDigest := writeFakeBinary(t, "probavi", "core bytes")

	fa := &fakeAdapter{probe: testProbe(), provRes: testProvision(), healthy: true, path: adapterPath}
	d, _ := newDrill(t, fa, &fakeProvider{sbx: &fakeSandbox{execValue: "1"}})
	d.Executable = func() (string, error) { return corePath, nil }

	rec, err := d.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rec.Adapter.Digest == nil || *rec.Adapter.Digest != adapterDigest {
		t.Errorf("adapter.digest = %v, want %q", deref(rec.Adapter.Digest), adapterDigest)
	}
	if rec.Env.ProbaviDigest == nil || *rec.Env.ProbaviDigest != coreDigest {
		t.Errorf("env.probavi_digest = %v, want %q", deref(rec.Env.ProbaviDigest), coreDigest)
	}
	// The version the writer emits, named rather than referenced: a test
	// asserting evidence.SchemaID against itself would pass through any
	// bump, and this one exists to notice one.
	if rec.Schema != "probavi-evidence/3" {
		t.Errorf("schema = %q, want probavi-evidence/3", rec.Schema)
	}
}

// TestUnreadableExecutablesStillLeaveARecord holds the rule §3 exists for:
// build identity is nullable so that a binary the process cannot hash never
// costs a drill its signed record. A digest is worth less than the proof.
func TestUnreadableExecutablesStillLeaveARecord(t *testing.T) {
	tests := []struct {
		name       string
		adapter    string
		executable func() (string, error)
	}{
		{"adapter path does not exist", filepath.Join(t.TempDir(), "gone"), func() (string, error) { return "", errors.New("no") }},
		{"executable cannot be resolved", "", func() (string, error) { return "", errors.New("unresolvable") }},
		{"executable resolves to nothing readable", "", func() (string, error) { return filepath.Join(t.TempDir(), "absent"), nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fa := &fakeAdapter{probe: testProbe(), provRes: testProvision(), healthy: true, path: tt.adapter}
			d, _ := newDrill(t, fa, &fakeProvider{sbx: &fakeSandbox{execValue: "1"}})
			d.Executable = tt.executable

			rec, err := d.Run(context.Background())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if rec.Outcome != "pass" {
				t.Errorf("outcome = %q, want pass — an unhashable binary is not a drill failure", rec.Outcome)
			}
			if rec.Adapter.Digest != nil {
				t.Errorf("adapter.digest = %q, want null", *rec.Adapter.Digest)
			}
			if rec.Env.ProbaviDigest != nil {
				t.Errorf("env.probavi_digest = %q, want null", *rec.Env.ProbaviDigest)
			}
			if err := rec.Validate(); err != nil {
				t.Errorf("a record with null digests must satisfy the schema: %v", err)
			}
		})
	}
}

// TestExecutableDefaultsToThisProcess covers the default wiring: a Drill
// nobody configured still records the binary that is running.
func TestExecutableDefaultsToThisProcess(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Skipf("this platform cannot resolve its own executable: %v", err)
	}
	d := &Drill{}
	d.defaults()
	got := d.selfDigest()
	if got == nil {
		t.Fatal("selfDigest returned nil for the test binary itself")
	}
	if want := evidenceDigestOf(t, self); *got != want {
		t.Errorf("digest = %q, want %q", *got, want)
	}
}

func evidenceDigestOf(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func deref(p *string) string {
	if p == nil {
		return "<null>"
	}
	return *p
}
