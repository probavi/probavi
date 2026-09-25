package checks

import (
	"testing"
	"time"
)

// TestMinutePrecisionTimestampsParse: Neo4j omits components that are
// zero, so an instant landing on a whole minute prints without seconds
// while every other instant prints with them. A list that read only the
// second form would pass for weeks and fail on a round minute, which is
// worse than never working at all.
func TestMinutePrecisionTimestampsParse(t *testing.T) {
	tests := map[string]string{
		"2026-09-24T18:30Z":       "2026-09-24T18:30:00Z",
		"2026-09-24T18:30+02:00":  "2026-09-24T16:30:00Z",
		"2026-09-24T18:30":        "2026-09-24T18:30:00Z",
		"2026-09-24T18:30:15Z":    "2026-09-24T18:30:15Z",
		"2026-09-24T18:30:15.25Z": "2026-09-24T18:30:15.25Z",
	}
	for in, want := range tests {
		got, err := parseTimestamp(in)
		if err != nil {
			t.Errorf("parseTimestamp(%q) = %v", in, err)
			continue
		}
		wantTime, err := time.Parse(time.RFC3339Nano, want)
		if err != nil {
			t.Fatalf("bad fixture %q: %v", want, err)
		}
		if !got.Equal(wantTime) {
			t.Errorf("parseTimestamp(%q) = %s, want %s", in, got.Format(time.RFC3339Nano), want)
		}
	}
	// A date alone is still not an instant: widening the list must not
	// widen it into accepting anything shaped vaguely like a time.
	for _, in := range []string{"2026-09-24", "18:30Z", "2026-09-24T18", ""} {
		if _, err := parseTimestamp(in); err == nil {
			t.Errorf("parseTimestamp(%q) succeeded, want a refusal", in)
		}
	}
}
