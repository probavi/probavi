package main

import (
	"strings"
	"testing"
)

func TestParseInstantCount(t *testing.T) {
	tests := []struct {
		name   string
		line   string
		want   int64
		wantOK bool
	}{
		{"the measured shape", "{} => 680 @[1786876374]", 680, true},
		{"zero", "{} => 0 @[1786876374]", 0, true},
		{"a labeled vector", `up{job='self'} => 1 @[1786876374.046]`, 1, true},
		{"an empty answer", "", 0, false},
		{"the simulated sandbox's stdout", "1", 0, false},
		{"a float value is not this count", "{} => 1.5 @[1]", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseInstantCount(tt.line)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("parseInstantCount(%q) = %d/%v, want %d/%v", tt.line, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestLastErrorLine(t *testing.T) {
	log := []byte(`time=2026-08-16T10:29:51.000Z level=INFO msg="Starting TSDB ..."
time=2026-08-16T10:29:51.383Z level=ERROR source=main.go:1618 msg="Fatal error" err="opening storage failed: reloadBlocks: corrupted block 01M051XQ: read symbols: invalid checksum"`)
	got := lastErrorLine(log)
	if !strings.Contains(got, "corrupted block") {
		t.Errorf("lastErrorLine = %q, want the server's own failure report", got)
	}
	if strings.Contains(got, `"`) {
		t.Errorf("lastErrorLine = %q must stay quote-free for protocol embedding", got)
	}
	if lastErrorLine([]byte("level=INFO msg=ready\n")) != "" {
		t.Error("a healthy log must yield nothing")
	}
}
