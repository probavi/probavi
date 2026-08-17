package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeAOFDir lays down an append-only-directory-shaped fixture: a
// manifest naming the given files, plus the files themselves. A file
// listed with empty content is still created; names in missing are
// named by the manifest but not written.
func writeAOFDir(t *testing.T, dir string, manifest string, files map[string]string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if manifest != "" {
		if err := os.WriteFile(filepath.Join(dir, "appendonly.aof.manifest"), []byte(manifest), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// healthyManifest is the two-member set a real 7.x server writes.
const healthyManifest = "file appendonly.aof.1.base.rdb seq 1 type b\n" +
	"file appendonly.aof.1.incr.aof seq 1 type i\n"

func healthyAOFFiles() map[string]string {
	return map[string]string{
		"appendonly.aof.1.base.rdb": string(rdbFixture([2]string{"redis-ver", "7.2.5"})),
		"appendonly.aof.1.incr.aof": "*1\r\n$4\r\nPING\r\n",
	}
}

// TestResolveAOFDirHealthySet pins what a real 7.x set resolves to: the
// manifest's members in manifest order, the base named, and the
// appendfilename derived from the manifest's own name.
func TestResolveAOFDirHealthySet(t *testing.T) {
	dir := writeAOFDir(t, filepath.Join(t.TempDir(), "appendonlydir"), healthyManifest, healthyAOFFiles())
	art, perr := resolveAOFDir(dir)
	if perr != nil {
		t.Fatalf("resolveAOFDir: %+v", perr)
	}
	if art.manifestName != "appendonly.aof.manifest" || art.baseName != "appendonly.aof.1.base.rdb" {
		t.Errorf("artifact = %+v", art)
	}
	if got := strings.Join(art.files, "|"); got != "appendonly.aof.1.base.rdb|appendonly.aof.1.incr.aof" {
		t.Errorf("files = %s, want manifest order", got)
	}
	if art.appendFilename() != "appendonly.aof" {
		t.Errorf("appendFilename = %q", art.appendFilename())
	}
}

// TestResolveAOFDirCustomAppendFilename proves history entries and a
// non-default appendfilename resolve too — the derived name is what
// keeps the restored server reading this set rather than a fresh one.
func TestResolveAOFDirCustomAppendFilename(t *testing.T) {
	manifest := "file custom.1.base.rdb seq 1 type b\n" +
		"file custom.1.incr.aof seq 1 type h\n" +
		"file custom.2.incr.aof seq 2 type i\n"
	dir := filepath.Join(t.TempDir(), "aof")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "custom.manifest"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"custom.1.base.rdb", "custom.1.incr.aof", "custom.2.incr.aof"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	art, perr := resolveAOFDir(dir)
	if perr != nil {
		t.Fatalf("resolveAOFDir: %+v", perr)
	}
	if art.appendFilename() != "custom" || len(art.files) != 3 {
		t.Errorf("artifact = %+v", art)
	}
}

func TestResolveAOFDirRefusals(t *testing.T) {
	base := t.TempDir()
	aofDir := func(name, manifest string, files map[string]string) string {
		return writeAOFDir(t, filepath.Join(base, name), manifest, files)
	}
	twoManifests := aofDir("two-manifests", healthyManifest, healthyAOFFiles())
	if err := os.WriteFile(filepath.Join(twoManifests, "other.manifest"), []byte(healthyManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	dataDirCopy := filepath.Join(base, "datadir")
	writeAOFDir(t, filepath.Join(dataDirCopy, "appendonlydir"), healthyManifest, healthyAOFFiles())
	legacyFile := filepath.Join(base, "appendonly.aof")
	if err := os.WriteFile(legacyFile, []byte("*1\r\n$4\r\nPING\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		path     string
		wantCode string
		wantMsg  string
	}{
		{
			// The gate this kind exists for: a mid-rewrite copy loses
			// members the manifest still names.
			"a manifest naming a missing file is an incomplete copy",
			aofDir("incomplete", healthyManifest, map[string]string{"appendonly.aof.1.base.rdb": "x"}),
			"source_corrupt", "appendonly.aof.1.incr.aof",
		},
		{
			"a directory without a manifest is not this artifact",
			aofDir("no-manifest", "", map[string]string{"dump.rdb": "REDIS0011"}),
			"source_corrupt", "no .manifest",
		},
		{
			"a malformed manifest line is damage",
			aofDir("malformed", "file appendonly.aof.1.base.rdb seq 1 type b\nnot a manifest line\n", healthyAOFFiles()),
			"source_corrupt", "line 2",
		},
		{
			"an empty manifest names no restore",
			aofDir("empty", "\n\n", healthyAOFFiles()),
			"source_corrupt", "names no files",
		},
		{
			"two base files are not a real set",
			aofDir("two-bases", "file a.base.rdb seq 1 type b\nfile b.base.rdb seq 2 type b\n",
				map[string]string{"a.base.rdb": "x", "b.base.rdb": "x"}),
			"source_corrupt", "two base files",
		},
		{
			"a quoted filename is refused rather than guessed at",
			aofDir("quoted", "file \"odd name.aof\" seq 1 type i\n", map[string]string{}),
			"source_corrupt", "line 1",
		},
		{"two manifests are ambiguity, not a choice", twoManifests, "source_corrupt", "2 manifest files"},
		{"a data-directory copy is redirected to its append-only directory",
			dataDirCopy, "invalid_request", filepath.Join(dataDirCopy, "appendonlydir")},
		{"a file for the kind teaches the 7+ directory", legacyFile, "invalid_request", "pre-7.0"},
		{"a missing path is source_not_found", filepath.Join(base, "gone"), "source_not_found", "does not exist"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, perr := resolveAOFDir(tt.path)
			if perr == nil || perr.Code != tt.wantCode || !strings.Contains(perr.Message, tt.wantMsg) {
				t.Fatalf("perr = %+v, want %s containing %q", perr, tt.wantCode, tt.wantMsg)
			}
		})
	}
}

// TestAOFChecksum pins the identity the evidence record carries: stable
// across stray neighbours, changed by any member change.
func TestAOFChecksum(t *testing.T) {
	dir := writeAOFDir(t, filepath.Join(t.TempDir(), "aof"), healthyManifest, healthyAOFFiles())
	art, perr := resolveAOFDir(dir)
	if perr != nil {
		t.Fatalf("resolveAOFDir: %+v", perr)
	}
	first, size, perr := aofChecksum(art)
	if perr != nil {
		t.Fatalf("aofChecksum: %+v", perr)
	}
	if !strings.HasPrefix(first, "sha256:") || size <= 0 {
		t.Fatalf("checksum = %q size = %d", first, size)
	}

	// A stray sidecar beside the set is not part of the restored bytes.
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte("sums\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	again, _, perr := aofChecksum(art)
	if perr != nil || again != first {
		t.Errorf("checksum moved on a stray sidecar: %q vs %q (%+v)", again, first, perr)
	}

	// A member change must move it.
	if err := os.WriteFile(filepath.Join(dir, "appendonly.aof.1.incr.aof"), []byte("*1\r\n$4\r\nQUIT\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	moved, _, perr := aofChecksum(art)
	if perr != nil || moved == first {
		t.Errorf("checksum did not move on a member change (%+v)", perr)
	}
}

// TestResolveAOFReadsTheBaseHead pins what the base RDB contributes:
// the origin version for the pre-check, the Valkey markers for the
// dialect fence — and no created_at, deliberately, because the base's
// ctime dates the last rewrite rather than the backup.
func TestResolveAOFReadsTheBaseHead(t *testing.T) {
	t.Run("redis-ver feeds the pre-check, ctime stays unreported", func(t *testing.T) {
		files := map[string]string{
			"appendonly.aof.1.base.rdb": string(rdbFixture(
				[2]string{"redis-ver", "7.2.5"}, [2]string{"ctime", "1755400000"})),
			"appendonly.aof.1.incr.aof": "*1\r\n$4\r\nPING\r\n",
		}
		dir := writeAOFDir(t, filepath.Join(t.TempDir(), "aof"), healthyManifest, files)
		src, perr := resolveSource(t.Context(), "redis_aof", dir)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		if src.redisVer != "7.2.5" || src.aof == nil {
			t.Errorf("src = %+v, want the base's redis-ver", src)
		}
		if src.createdAt != nil {
			t.Errorf("created_at = %q, want nil — the base ctime dates the rewrite, not the backup", *src.createdAt)
		}
	})

	t.Run("a Valkey base is refused by name", func(t *testing.T) {
		files := map[string]string{
			"appendonly.aof.1.base.rdb": string(rdbFixture([2]string{"valkey-ver", "8.0.1"})),
			"appendonly.aof.1.incr.aof": "*1\r\n$4\r\nPING\r\n",
		}
		dir := writeAOFDir(t, filepath.Join(t.TempDir(), "aof"), healthyManifest, files)
		_, perr := resolveSource(t.Context(), "redis_aof", dir)
		if perr == nil || perr.Code != "unsupported_source" || !strings.Contains(perr.Message, "Valkey") {
			t.Fatalf("perr = %+v, want the dialect fence", perr)
		}
	})

	t.Run("a plain-text base contributes nothing and refuses nothing", func(t *testing.T) {
		manifest := "file appendonly.aof.1.base.aof seq 1 type b\n" +
			"file appendonly.aof.1.incr.aof seq 1 type i\n"
		files := map[string]string{
			"appendonly.aof.1.base.aof": "*1\r\n$6\r\nSELECT\r\n",
			"appendonly.aof.1.incr.aof": "*1\r\n$4\r\nPING\r\n",
		}
		dir := writeAOFDir(t, filepath.Join(t.TempDir(), "aof"), manifest, files)
		src, perr := resolveSource(t.Context(), "redis_aof", dir)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		if src.redisVer != "" || src.createdAt != nil {
			t.Errorf("src = %+v, want no metadata from a plain-text base", src)
		}
	})
}
