package adapter

import "testing"

// TestHighestCommonPicksTheHighestBothSpeak is §8's negotiation rule, in
// both directions: a v1 adapter is driven at v1, and a v0 adapter — every
// adapter in the catalogue today — is driven at the floor, unchanged.
func TestHighestCommonPicksTheHighestBothSpeak(t *testing.T) {
	tests := []struct {
		name   string
		theirs []string
		want   string
	}{
		{"speaks both", []string{ProtocolFloor, ProtocolVersion}, ProtocolVersion},
		{"speaks only the floor", []string{ProtocolFloor}, ProtocolFloor},
		{"speaks only v1", []string{ProtocolVersion}, ProtocolVersion},
		{"order does not matter", []string{ProtocolVersion, ProtocolFloor}, ProtocolVersion},
		{"speaks neither", []string{"probavi-adapter/99"}, ""},
		{"speaks nothing", nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := highestCommon(tc.theirs); got != tc.want {
				t.Errorf("highestCommon(%v) = %q, want %q", tc.theirs, got, tc.want)
			}
		})
	}
}

// TestProtocolIsTheFloorUntilAProbeAnswers: the probe request carries the
// floor, so a drill that got no further spoke exactly that, and the record
// must not claim more.
func TestProtocolIsTheFloorUntilAProbeAnswers(t *testing.T) {
	r := &Runner{}
	if got := r.Protocol(); got != ProtocolFloor {
		t.Errorf("Protocol() = %q before a probe, want the floor %q", got, ProtocolFloor)
	}
	r.negotiated = ProtocolVersion
	if got := r.Protocol(); got != ProtocolVersion {
		t.Errorf("Protocol() = %q after negotiation, want %q", got, ProtocolVersion)
	}
}

// TestProtocolVersionsIsACopy: the published list is the protocol's, not a
// handle onto the package's own slice.
func TestProtocolVersionsIsACopy(t *testing.T) {
	got := ProtocolVersions()
	if len(got) == 0 || got[0] != ProtocolVersion {
		t.Fatalf("ProtocolVersions() = %v, want the highest first", got)
	}
	got[0] = "probavi-adapter/999"
	if ProtocolVersions()[0] != ProtocolVersion {
		t.Error("a caller rewrote the package's version list")
	}
}
