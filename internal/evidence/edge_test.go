package evidence

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// invalidUTF8 is a byte sequence that is not valid UTF-8.
var invalidUTF8 = string([]byte{0xff, 0xfe})

func TestValidateRejectsInvalidUTF8(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(r *Record)
	}{
		{"drill name", func(r *Record) { r.Drill.Name = invalidUTF8 }},
		{"adapter version", func(r *Record) { r.Adapter.Version = &invalidUTF8 }},
		{"check name", func(r *Record) { r.Checks[0].Name = invalidUTF8 }},
		{"check detail", func(r *Record) { r.Checks[0].Detail = &invalidUTF8 }},
		{"params key", func(r *Record) { r.Sandbox.Params[invalidUTF8] = "v" }},
		{"params value", func(r *Record) { r.Sandbox.Params["k"] = invalidUTF8 }},
		{"error message", func(r *Record) {
			r.Outcome = OutcomeFail
			r.Error = &DrillError{Code: "x", Message: invalidUTF8}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := sampleRecordPass()
			tt.mutate(rec)
			if err := rec.Validate(); err == nil {
				t.Error("Validate accepted invalid UTF-8 — json.Marshal would silently rewrite it as U+FFFD before signing")
			}
		})
	}
}

func TestParamsSeparatorCannotFalseAccept(t *testing.T) {
	// 0xc3 alone and 0xa9 alone are invalid UTF-8, but concatenated they
	// form "é"; the NUL join must still reject the pair.
	rec := sampleRecordPass()
	rec.Sandbox.Params[string([]byte{0xc3})] = string([]byte{0xa9})
	if err := rec.Validate(); err == nil {
		t.Error("Validate accepted params whose key+value only look valid when concatenated")
	}
}

func TestCanonicalizeObjectEdges(t *testing.T) {
	got, err := Canonicalize(map[string]any{"ab": json.Number("2"), "a": json.Number("1")})
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if string(got) != `{"a":1,"ab":2}` {
		t.Errorf("prefix key ordering = %s, want {\"a\":1,\"ab\":2}", got)
	}
	if _, err := Canonicalize(map[string]any{invalidUTF8: json.Number("1")}); err == nil {
		t.Error("Canonicalize accepted an invalid UTF-8 object key")
	}
}

func TestLoadKeyReadFailures(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keydir")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := LoadSigner(dir); err == nil {
		t.Error("LoadSigner accepted a directory")
	}
	if _, err := LoadPublicKey(filepath.Join(t.TempDir(), "missing.pub")); err == nil {
		t.Error("LoadPublicKey accepted a missing file")
	}
}

func TestOpenFailures(t *testing.T) {
	if _, err := Open(t.TempDir(), testSigner(), nil); err == nil {
		t.Error("Open accepted a directory as log path")
	}
	missing := filepath.Join(t.TempDir(), "no", "such", "dir", "e.jsonl")
	if _, err := Open(missing, testSigner(), nil); err == nil {
		t.Error("Open accepted an uncreatable lock path")
	}
}

func TestStorePoisonedAfterWriteFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence.jsonl")
	st, err := Open(path, testSigner(), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := st.Close(); err == nil {
		t.Error("second Close: expected error")
	}

	// Writing through a closed file must fail and poison the store.
	if err := st.Append(sampleRecordPass()); err == nil {
		t.Fatal("Append on closed store: expected error")
	}
	if err := st.Append(sampleRecordPass()); err == nil || !strings.Contains(err.Error(), "failed state") {
		t.Errorf("Append on poisoned store: got %v, want failed-state refusal", err)
	}
}

func TestVerifySignatureShapeTampering(t *testing.T) {
	path := buildLog(t)

	tests := []struct {
		name       string
		mutate     func(m map[string]any)
		wantReason string
	}{
		{"missing sig", func(m map[string]any) { delete(m, "sig") }, "no sig"},
		{"malformed sig_b64", func(m map[string]any) {
			asMap(t, m["sig"])["sig_b64"] = "!!!not-base64!!!"
		}, "malformed sig.sig_b64"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines := logLines(t, path)
			lines[0] = mutateLine(t, lines[0], tt.mutate)
			res, err := Verify(strings.NewReader(strings.Join(lines, "\n")+"\n"), testKeyring(), nil)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if res.Status != StatusInvalid || !strings.Contains(res.Reason, tt.wantReason) {
				t.Errorf("Verify = %s (%q), want INVALID with reason containing %q", res.Status, res.Reason, tt.wantReason)
			}
		})
	}
}

func TestVerifyRejectsOversizedLine(t *testing.T) {
	line := `{"a":"` + strings.Repeat("x", MaxRecordBytes) + `"}` + "\n"
	res, err := Verify(strings.NewReader(line), testKeyring(), nil)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Status != StatusInvalid || !strings.Contains(res.Reason, "maximum canonical size") {
		t.Errorf("Verify = %s (%q), want INVALID for oversized record", res.Status, res.Reason)
	}
}
