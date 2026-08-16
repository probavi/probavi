package evidence

import (
	"errors"
	"strings"
	"testing"
)

func strPtr(s string) *string { return &s }
func i64Ptr(n int64) *int64   { return &n }

func hexRef(pair string) string { return "sha256:" + strings.Repeat(pair, 32) }

// sampleRecordPass is a fully populated passing record (a PITR drill, so
// the golden log covers drill.pitr_target with a value; the error sample
// covers null).
func sampleRecordPass() *Record {
	return &Record{
		Schema: SchemaID,
		TS:     "2026-07-31T02:00:11.482Z",
		Drill: Drill{
			Name: "prod-orders-db", ConfigHash: hexRef("7d"),
			PITRTarget: strPtr("2026-07-30T14:32:00.000Z"),
		},
		Backup: Backup{
			Kind:      "pgdump",
			Checksum:  strPtr(hexRef("9f")),
			SizeBytes: i64Ptr(565248),
			CreatedAt: strPtr("2026-07-30T01:58:02.000Z"),
		},
		Adapter: Adapter{
			Name: "postgres", Version: strPtr("0.1.0"), Protocol: "probavi-adapter/0",
			Digest: strPtr(hexRef("4c")),
		},
		Sandbox: Sandbox{Provider: "docker", Params: map[string]string{"image": "postgres:16", "memory": "2GiB"}},
		Timings: Timings{
			Provision: i64Ptr(1170), EngineReady: i64Ptr(1166), Transfer: i64Ptr(110),
			Restore: i64Ptr(190), Validate: i64Ptr(61), Total: i64Ptr(2840),
		},
		Checks: []Check{
			{Name: "service_healthy", OK: true, Detail: strPtr("accepting connections")},
			{Name: "row_count:orders", OK: true, Detail: strPtr("100000 rows (min 100000)")},
		},
		Outcome: OutcomePass,
		Error:   nil,
		Env: Env{
			ProbaviVersion: "0.1.0", OS: "linux", Arch: "amd64", HostID: "3f7a9c2e5b1d8e04",
			ProbaviDigest: strPtr(hexRef("1d")),
		},
	}
}

// sampleRecordFail reached a negative recoverability verdict.
func sampleRecordFail() *Record {
	rec := sampleRecordPass()
	rec.TS = "2026-07-31T02:05:00.000Z"
	rec.Checks = []Check{{Name: "row_count:orders", OK: false, Detail: strPtr("99000 rows (min 100000)")}}
	rec.Outcome = OutcomeFail
	rec.Error = &DrillError{Code: "check_failed", Message: "1 of 1 checks failed"}
	return rec
}

// sampleRecordError is an infrastructure failure: most fields unknowable.
func sampleRecordError() *Record {
	return &Record{
		Schema:  SchemaID,
		TS:      "2026-07-31T02:10:30.019Z",
		Drill:   Drill{Name: "prod-orders-db", ConfigHash: hexRef("7d")},
		Backup:  Backup{Kind: "pgdump", Checksum: nil, SizeBytes: nil, CreatedAt: nil},
		Adapter: Adapter{Name: "postgres", Version: nil, Protocol: "probavi-adapter/0"},
		Sandbox: Sandbox{Provider: "docker", Params: map[string]string{"image": "postgres:16"}},
		Timings: Timings{Provision: i64Ptr(3000), Total: i64Ptr(3105)},
		Checks:  []Check{},
		Outcome: OutcomeError,
		Error:   &DrillError{Code: "sandbox_error", Message: "sandbox runtime died during provisioning"},
		Env: Env{
			ProbaviVersion: "0.1.0", OS: "linux", Arch: "amd64", HostID: "3f7a9c2e5b1d8e04",
			ProbaviDigest: strPtr(hexRef("1d")),
		},
	}
}

func TestValidateAcceptsSamples(t *testing.T) {
	for name, rec := range map[string]*Record{
		"pass": sampleRecordPass(), "fail": sampleRecordFail(), "error": sampleRecordError(),
	} {
		if err := rec.Validate(); err != nil {
			t.Errorf("Validate(%s): %v", name, err)
		}
	}
}

func TestValidateRejects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(r *Record)
	}{
		{"wrong schema", func(r *Record) { r.Schema = "probavi-evidence/99" }},
		{"non-UTC ts", func(r *Record) { r.TS = "2026-07-31T02:00:11.482+02:00" }},
		{"second-precision ts", func(r *Record) { r.TS = "2026-07-31T02:00:11Z" }},
		{"empty drill name", func(r *Record) { r.Drill.Name = "" }},
		{"bad config hash", func(r *Record) { r.Drill.ConfigHash = "md5:abc" }},
		{"bad pitr target", func(r *Record) { r.Drill.PITRTarget = strPtr("yesterday 14:32") }},
		{"second-precision pitr target", func(r *Record) { r.Drill.PITRTarget = strPtr("2026-07-30T14:32:00Z") }},
		{"unknown outcome", func(r *Record) { r.Outcome = "maybe" }},
		{"pass with error", func(r *Record) { r.Error = &DrillError{Code: "x", Message: "y"} }},
		{"fail without error", func(r *Record) { r.Outcome = OutcomeFail }},
		{"empty error code", func(r *Record) {
			r.Outcome = OutcomeFail
			r.Error = &DrillError{Code: "", Message: "y"}
		}},
		{"oversized error message", func(r *Record) {
			r.Outcome = OutcomeFail
			r.Error = &DrillError{Code: "x", Message: strings.Repeat("m", maxErrorMessageLen+1)}
		}},
		{"multiline error message", func(r *Record) {
			r.Outcome = OutcomeFail
			r.Error = &DrillError{Code: "x", Message: "line1\nline2"}
		}},
		{"nil checks", func(r *Record) { r.Checks = nil }},
		{"empty check name", func(r *Record) { r.Checks[0].Name = "" }},
		{"oversized check detail", func(r *Record) { r.Checks[0].Detail = strPtr(strings.Repeat("d", maxDetailLen+1)) }},
		{"multiline check detail", func(r *Record) { r.Checks[0].Detail = strPtr("a\nb") }},
		{"empty backup kind", func(r *Record) { r.Backup.Kind = "" }},
		{"bad backup checksum", func(r *Record) { r.Backup.Checksum = strPtr("sha256:short") }},
		{"bad backup created_at", func(r *Record) { r.Backup.CreatedAt = strPtr("yesterday") }},
		{"negative backup size", func(r *Record) { r.Backup.SizeBytes = i64Ptr(-1) }},
		{"empty adapter name", func(r *Record) { r.Adapter.Name = "" }},
		{"empty adapter protocol", func(r *Record) { r.Adapter.Protocol = "" }},
		{"bad adapter digest", func(r *Record) { r.Adapter.Digest = strPtr("sha256:short") }},
		{"empty sandbox provider", func(r *Record) { r.Sandbox.Provider = "" }},
		{"nil sandbox params", func(r *Record) { r.Sandbox.Params = nil }},
		{"negative timing", func(r *Record) { r.Timings.Restore = i64Ptr(-5) }},
		{"empty env version", func(r *Record) { r.Env.ProbaviVersion = "" }},
		{"bad host id", func(r *Record) { r.Env.HostID = "NOT-HEX" }},
		{"bad probavi digest", func(r *Record) { r.Env.ProbaviDigest = strPtr("md5:abc") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := sampleRecordPass()
			tt.mutate(rec)
			if err := rec.Validate(); !errors.Is(err, ErrInvalidRecord) {
				t.Errorf("Validate: got %v, want ErrInvalidRecord", err)
			}
		})
	}
}

// TestTimingsValidateNamesAFixedField pins the diagnostic's determinism.
// With several negative phases a map-ranged loop named a different one on
// each run, which is a poor property for text a trust product prints and a
// reader may be comparing between two logs.
func TestTimingsValidateNamesAFixedField(t *testing.T) {
	neg := int64(-1)
	all := Timings{Provision: &neg, EngineReady: &neg, Transfer: &neg, Restore: &neg, Validate: &neg, Total: &neg}
	first := all.validate()
	if first == nil {
		t.Fatal("negative timings must be rejected")
	}
	for range 50 {
		if got := all.validate(); got.Error() != first.Error() {
			t.Fatalf("diagnostic varies between runs: %v vs %v", got, first)
		}
	}
	if !strings.Contains(first.Error(), "timings_ms.provision") {
		t.Errorf("error = %v, want the first field in declaration order", first)
	}
}
