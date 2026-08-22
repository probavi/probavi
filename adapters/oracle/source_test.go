package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveSourceMeasuresTheFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "orders.dmp")
	content := []byte("not a dump, and the host must not say so")
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatal(err)
	}
	src, perr := resolveSource("oracle_datapump", p)
	if perr != nil {
		t.Fatalf("resolveSource: %+v", perr)
	}
	sum := sha256.Sum256(content)
	if src.checksum != "sha256:"+hex.EncodeToString(sum[:]) || src.sizeBytes != int64(len(content)) || src.path != p {
		t.Errorf("source = %+v", src)
	}
}

func TestResolveSourceRefusals(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.dmp")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, kind, path, wantCode string
	}{
		{"unknown kind", "rman_backupset", empty, "unsupported_source"},
		{"missing", "oracle_datapump", filepath.Join(dir, "nope.dmp"), "source_not_found"},
		{"directory", "oracle_datapump", dir, "invalid_request"},
		{"empty", "oracle_datapump", empty, "source_corrupt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src, perr := resolveSource(tt.kind, tt.path)
			if src != nil || perr == nil || perr.Code != tt.wantCode {
				t.Errorf("resolveSource = %+v, %+v; want code %s", src, perr, tt.wantCode)
			}
		})
	}
}

func TestBackupLocation(t *testing.T) {
	if loc, perr := backupLocation(nil); loc != nil || perr != nil {
		t.Errorf("no declaration: %v %+v", loc, perr)
	}
	if loc, perr := backupLocation(map[string]string{backupTimezoneParam: "Europe/Budapest"}); perr != nil || loc == nil || loc.String() != "Europe/Budapest" {
		t.Errorf("Europe/Budapest: %v %+v", loc, perr)
	}
	if _, perr := backupLocation(map[string]string{backupTimezoneParam: "Nowhere/Here"}); perr == nil || perr.Code != "invalid_request" {
		t.Errorf("unknown zone: %+v", perr)
	}
}
