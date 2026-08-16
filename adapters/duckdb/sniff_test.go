package main

import (
	"testing"
)

func TestParseDuckHeader(t *testing.T) {
	tests := []struct {
		name string
		head []byte
		want duckHeader
	}{
		{"a default 1.5.5 file", duckFixture(64, "v1.5.5"),
			duckHeader{valid: true, storageVersion: 64, libraryVersion: "v1.5.5"}},
		{"a newer storage format", duckFixture(68, "v1.5.5"),
			duckHeader{valid: true, storageVersion: 68, libraryVersion: "v1.5.5"}},
		{"a header without a version string", duckFixture(64, ""),
			duckHeader{valid: true, storageVersion: 64}},
		{"noise where the version string lives", duckFixture(64, "not-a-version"),
			duckHeader{valid: true, storageVersion: 64}},
		{"a head cut before the version string", duckFixture(64, "v1.5.5")[:32],
			duckHeader{valid: true, storageVersion: 64}},
		{"a head cut inside the storage version", duckFixture(64, "v1.5.5")[:14],
			duckHeader{valid: true}},
		{"a head cut inside the magic", duckFixture(64, "v1.5.5")[:10], duckHeader{}},
		{"sql text", []byte("CREATE TABLE t(id INTEGER);\n"), duckHeader{}},
		{"empty", nil, duckHeader{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseDuckHeader(tt.head); got != tt.want {
				t.Errorf("parseDuckHeader = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestIsGzip(t *testing.T) {
	if !isGzip([]byte{0x1f, 0x8b, 0x08}) {
		t.Error("gzip header not recognised")
	}
	if isGzip([]byte{0x1f}) || isGzip(duckFixture(64, "v1.5.5")) || isGzip(nil) {
		t.Error("non-gzip bytes recognised as gzip")
	}
}
