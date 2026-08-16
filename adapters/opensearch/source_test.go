package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveSourceKindGates(t *testing.T) {
	base := t.TempDir()
	repo := writeRepo(t, filepath.Join(base, "repo"))
	archive := treeToTar(t, repo, filepath.Join(base, "repo.tar"), "", false)

	tests := []struct {
		name     string
		kind     string
		path     string
		wantCode string
	}{
		{"unknown kind", "opensearch_backup", repo, "unsupported_source"},
		{"missing archive", "opensearch_repo_tar", filepath.Join(base, "gone.tar"), "source_not_found"},
		{"missing directory", "opensearch_repo", filepath.Join(base, "gone"), "source_not_found"},
		{"a directory for the tar kind", "opensearch_repo_tar", repo, "invalid_request"},
		{"a file for the directory kind", "opensearch_repo", archive, "invalid_request"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, perr := resolveSource(tc.kind, tc.path); perr == nil || perr.Code != tc.wantCode {
				t.Errorf("verdict = %+v, want %s", perr, tc.wantCode)
			}
		})
	}
}

func TestResolveTarCarriesTheCensus(t *testing.T) {
	repo := writeRepo(t, t.TempDir())
	path := treeToTar(t, repo, filepath.Join(t.TempDir(), "repo.tar"), "", false)
	src, perr := resolveSource("opensearch_repo_tar", path)
	if perr != nil {
		t.Fatalf("resolveSource: %+v", perr)
	}
	if !src.tarball || !strings.HasPrefix(src.checksum, "sha256:") || src.sizeBytes <= 0 {
		t.Errorf("src = %+v, want a measured archive identity", src)
	}
	if len(src.census.snapshots) != 2 {
		t.Errorf("census = %+v, want the archive's own two snapshots", src.census)
	}
}

func TestResolveTarStaysSilentOnOpaqueBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opaque.bin")
	writeFixtureFile(t, path, []byte(strings.Repeat("x", 4096)))
	src, perr := resolveSource("opensearch_repo_tar", path)
	if perr != nil {
		t.Fatalf("resolveSource: %+v — the sandbox extraction is the authority on an opaque archive", perr)
	}
	if len(src.census.snapshots) != 0 {
		t.Errorf("census = %+v, want no claims out of unreadable bytes", src.census)
	}
}

func TestResolveRepoDirVetsAndMeasures(t *testing.T) {
	repo := writeRepo(t, t.TempDir())
	src, perr := resolveSource("opensearch_repo", repo)
	if perr != nil {
		t.Fatalf("resolveSource: %+v", perr)
	}
	if src.tarball || !strings.HasPrefix(src.checksum, "sha256:") || src.sizeBytes <= 0 {
		t.Errorf("src = %+v, want a measured tree identity", src)
	}
	if got := src.census.names(); len(got) != 2 || got[0] != "snap-1" {
		t.Errorf("census names = %v", got)
	}
}

func TestDirChecksumIsCanonical(t *testing.T) {
	dir := writeRepo(t, t.TempDir())
	first, size, perr := dirChecksum(dir)
	if perr != nil {
		t.Fatalf("dirChecksum: %+v", perr)
	}
	if size <= 0 {
		t.Errorf("size = %d", size)
	}
	again, _, perr := dirChecksum(dir)
	if perr != nil || again != first {
		t.Errorf("checksum not deterministic: %q vs %q (%+v)", first, again, perr)
	}
	writeFixtureFile(t, filepath.Join(dir, "indices", "i1", "0", "__blob"), []byte("changed!!!"))
	changed, _, perr := dirChecksum(dir)
	if perr != nil || changed == first {
		t.Error("a content change must change the checksum")
	}
	if _, _, perr := dirChecksum(t.TempDir()); perr == nil || perr.Code != "source_not_found" {
		t.Errorf("empty tree verdict = %+v, want source_not_found", perr)
	}
}

func TestDirChecksumCoversSymlinks(t *testing.T) {
	dir := writeRepo(t, t.TempDir())
	if err := os.Symlink("index-0", filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	withLink, _, perr := dirChecksum(dir)
	if perr != nil {
		t.Fatalf("dirChecksum: %+v", perr)
	}
	if err := os.Remove(filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("index.latest", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	retargeted, _, perr := dirChecksum(dir)
	if perr != nil || retargeted == withLink {
		t.Error("a retargeted symlink must change the checksum")
	}
}

func TestFormatCreatedAt(t *testing.T) {
	if got := formatCreatedAt(0); got != nil {
		t.Errorf("formatCreatedAt(0) = %v, want nil — no instant, no claim", *got)
	}
	got := formatCreatedAt(1700000100000)
	if got == nil || *got != "2023-11-14T22:15:00.000Z" {
		t.Errorf("formatCreatedAt = %v, want the exact UTC instant", got)
	}
}

func TestRejectBackupTimezone(t *testing.T) {
	if perr := rejectBackupTimezone(nil); perr != nil {
		t.Errorf("nil params: %+v", perr)
	}
	if perr := rejectBackupTimezone(map[string]string{"other": "x"}); perr != nil {
		t.Errorf("unrelated params: %+v", perr)
	}
	perr := rejectBackupTimezone(map[string]string{backupTimezoneParam: "Europe/Budapest"})
	if perr == nil || perr.Code != "invalid_request" ||
		!strings.Contains(perr.Message, backupTimezoneParam) {
		t.Errorf("verdict = %+v, want invalid_request naming the parameter", perr)
	}
}
