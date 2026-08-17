package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// manifestJSON builds a v2 manifest naming one KV file, one SQL file,
// and the given org/bucket/shard triples.
func manifestJSON(stem string, shards []string, orgBuckets map[string][]string) string {
	buckets := []string{}
	i := 0
	for org, names := range orgBuckets {
		for _, name := range names {
			shard := ""
			if i < len(shards) {
				shard = fmt.Sprintf(`{"shards":[{"id":%d,"fileName":%q}]}`, i+1, shards[i])
			} else {
				shard = `{"shards":[]}`
			}
			buckets = append(buckets, fmt.Sprintf(
				`{"organizationName":%q,"bucketName":%q,"retentionPolicies":[{"shardGroups":[%s]}]}`,
				org, name, shard))
			i++
		}
	}
	return fmt.Sprintf(`{"manifestVersion":2,"kv":{"fileName":%q,"size":10},"sql":{"fileName":%q,"size":10},"buckets":[%s]}`,
		stem+".bolt.gz", stem+".sqlite.gz", strings.Join(buckets, ","))
}

// writeBackup lays down a backup-directory-shaped fixture: the manifest
// plus the files it names (unless withheld).
func writeBackup(t *testing.T, dir, stem string, orgBuckets map[string][]string, withhold ...string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	shards := []string{}
	n := 0
	for _, names := range orgBuckets {
		n += len(names)
	}
	for i := 1; i <= n; i++ {
		shards = append(shards, fmt.Sprintf("%s.%d.tar.gz", stem, i))
	}
	held := map[string]bool{}
	for _, name := range withhold {
		held[name] = true
	}
	files := append([]string{stem + ".bolt.gz", stem + ".sqlite.gz"}, shards...)
	for _, name := range files {
		if held[name] {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("member: "+name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest := manifestJSON(stem, shards, orgBuckets)
	if err := os.WriteFile(filepath.Join(dir, stem+".manifest"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

const stemA = "20260817T100000Z"

func singleOrg() map[string][]string {
	return map[string][]string{"probavi-org": {"metrics", "events"}}
}

func TestResolveBackupDirHealthy(t *testing.T) {
	{
		dir := writeBackup(t, filepath.Join(t.TempDir(), "bak"), stemA, singleOrg())
		src, perr := resolveSource("influx_backup", dir)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		if src.manifestName != stemA+".manifest" || len(src.files) != 5 {
			t.Errorf("src = manifest %q files %v", src.manifestName, src.files)
		}
		if src.createdAt == nil || *src.createdAt != "2026-08-17T10:00:00.000Z" {
			t.Errorf("created_at = %v, want the stem's own instant", src.createdAt)
		}
		if !strings.HasPrefix(src.checksum, "sha256:") || src.sizeBytes <= 0 {
			t.Errorf("identity = %q / %d", src.checksum, src.sizeBytes)
		}
		if got := strings.Join(src.orgs["probavi-org"], "|"); got != "metrics|events" {
			t.Errorf("orgs = %+v", src.orgs)
		}
	}
}

// TestResolveBackupDirUnparsableStem proves a renamed set still
// resolves, undated — nothing is invented.
func TestResolveBackupDirUnparsableStem(t *testing.T) {
	{
		dir := filepath.Join(t.TempDir(), "bak")
		writeBackup(t, dir, stemA, singleOrg())
		for _, name := range []string{".manifest", ".bolt.gz", ".sqlite.gz", ".1.tar.gz", ".2.tar.gz"} {
			if err := os.Rename(filepath.Join(dir, stemA+name), filepath.Join(dir, "renamed"+name)); err != nil {
				t.Fatal(err)
			}
		}
		// The manifest still names the original member names, so rebuild
		// it around the renamed ones.
		manifest := strings.ReplaceAll(manifestJSON(stemA, []string{stemA + ".1.tar.gz", stemA + ".2.tar.gz"}, singleOrg()), stemA, "renamed")
		if err := os.WriteFile(filepath.Join(dir, "renamed.manifest"), []byte(manifest), 0o600); err != nil {
			t.Fatal(err)
		}
		src, perr := resolveSource("influx_backup", dir)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		if src.createdAt != nil {
			t.Errorf("created_at = %q, want nil for a stem that does not parse", *src.createdAt)
		}
	}
}

// TestResolveBackupDirReusedTarget proves the stem ranking inside one
// directory, and that the neighbour set stays out of the identity.
func TestResolveBackupDirReusedTarget(t *testing.T) {
	{
		dir := filepath.Join(t.TempDir(), "bak")
		writeBackup(t, dir, "20260810T000000Z", singleOrg())
		writeBackup(t, dir, stemA, singleOrg())
		src, perr := resolveSource("influx_backup", dir)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		if src.manifestName != stemA+".manifest" {
			t.Errorf("chose %q, want the newest stem", src.manifestName)
		}
		// The neighbour set's members must not enter the identity.
		for _, f := range src.files {
			if strings.HasPrefix(f, "20260810") {
				t.Errorf("files include the older backup's %s", f)
			}
		}
	}
}

// writeRawManifestDir lays down a directory holding one raw manifest.
func writeRawManifestDir(t *testing.T, base, name, manifest string) string {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stemA+manifestSuffix), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestResolveBackupDirRefusals(t *testing.T) {
	base := t.TempDir()
	missingShard := writeBackup(t, filepath.Join(base, "incomplete"), stemA, singleOrg(),
		stemA+".2.tar.gz")
	missingKV := writeBackup(t, filepath.Join(base, "no-kv"), stemA, singleOrg(), stemA+".bolt.gz")

	empty := filepath.Join(base, "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	notDir := filepath.Join(base, "file")
	if err := os.WriteFile(notDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The 1.x portable manifest, verbatim shape (measured on 1.12.4).
	portable := writeRawManifestDir(t, base, "portable",
		`{"meta":{"fileName":"`+stemA+`.meta","size":66},"limited":false,"files":null}`)
	futureVersion := writeRawManifestDir(t, base, "future",
		`{"manifestVersion":3,"kv":{"fileName":"x","size":1},"buckets":[{"organizationName":"o","bucketName":"b","retentionPolicies":[]}]}`)
	noBuckets := writeRawManifestDir(t, base, "no-buckets",
		`{"manifestVersion":2,"kv":{"fileName":"k","size":1},"buckets":[]}`)
	mixedStems := writeBackup(t, filepath.Join(base, "mixed"), stemA, singleOrg())
	if err := os.WriteFile(filepath.Join(mixedStems, "renamed.manifest"),
		[]byte(manifestJSON(stemA, nil, singleOrg())), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		path     string
		wantCode string
		wantMsg  string
	}{
		{
			// The gate this format needs most: a partial copy loses shard
			// files the manifest still names.
			"a manifest naming a missing shard is an incomplete copy",
			missingShard, "source_corrupt", stemA + ".2.tar.gz",
		},
		{"a missing KV store file is an incomplete copy", missingKV, "source_corrupt", ".bolt.gz"},
		{"a directory without a manifest is not this artifact", empty, "source_corrupt", "no .manifest"},
		{"a file for the kind teaches the directory shape", notDir, "invalid_request", "influx backup"},
		{
			// The ROADMAP mandate: 1.x → 2.x is a migration, refused by
			// name rather than failing as corrupt.
			"a 1.x portable backup is a migration, not a restore",
			portable, "unsupported_source", "migration",
		},
		{"an unverified manifest version is refused", futureVersion, "unsupported_source", "version 3"},
		{"several manifests with an undated one is ambiguity", mixedStems, "source_corrupt", "renamed.manifest"},
		{"a manifest naming no buckets proves nothing", noBuckets, "source_corrupt", "no buckets"},
		{"a missing path is source_not_found", filepath.Join(base, "gone"), "source_not_found", "does not exist"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, perr := resolveSource("influx_backup", tt.path)
			if perr == nil || perr.Code != tt.wantCode || !strings.Contains(perr.Message, tt.wantMsg) {
				t.Fatalf("perr = %+v, want %s containing %q", perr, tt.wantCode, tt.wantMsg)
			}
		})
	}

	t.Run("an unknown kind lists the supported ones", func(t *testing.T) {
		_, perr := resolveSource("influx_snapshot", base)
		if perr == nil || perr.Code != "unsupported_source" || !strings.Contains(perr.Message, "influx_backup_dir") {
			t.Fatalf("perr = %+v", perr)
		}
	})
}

func TestNewestBackupIn(t *testing.T) {
	t.Run("the newest backup by its own stem wins over file times", func(t *testing.T) {
		base := t.TempDir()
		writeBackup(t, filepath.Join(base, "a-old"), "20260810T000000Z", singleOrg())
		writeBackup(t, filepath.Join(base, "b-new"), stemA, singleOrg())
		// A decoy: the newest backup's directory looks stale on disk.
		past := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
		if err := os.Chtimes(filepath.Join(base, "b-new"), past, past); err != nil {
			t.Fatal(err)
		}
		src, perr := resolveSource("influx_backup_dir", base)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		if filepath.Base(src.dir) != "b-new" {
			t.Errorf("chose %s, want the backup whose own stem is newest", src.dir)
		}
	})

	t.Run("subdirectories without a manifest are passed over, counted", func(t *testing.T) {
		base := t.TempDir()
		if err := os.MkdirAll(filepath.Join(base, "not-a-backup"), 0o755); err != nil {
			t.Fatal(err)
		}
		_, perr := resolveSource("influx_backup_dir", base)
		if perr == nil || perr.Code != "source_not_found" || !strings.Contains(perr.Message, "passed over") {
			t.Fatalf("perr = %+v", perr)
		}
	})
}

func TestMemberChecksumMoves(t *testing.T) {
	dir := writeBackup(t, filepath.Join(t.TempDir(), "bak"), stemA, singleOrg())
	src, perr := resolveSource("influx_backup", dir)
	if perr != nil {
		t.Fatalf("resolveSource: %+v", perr)
	}
	first := src.checksum

	// A stray sidecar beside the set is not part of the restored bytes.
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte("sums\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	again, perr := resolveSource("influx_backup", dir)
	if perr != nil || again.checksum != first {
		t.Errorf("checksum moved on a stray sidecar: %q vs %q (%+v)", again.checksum, first, perr)
	}

	// A member change must move it.
	if err := os.WriteFile(filepath.Join(dir, stemA+".1.tar.gz"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	moved, perr := resolveSource("influx_backup", dir)
	if perr != nil || moved.checksum == first {
		t.Errorf("checksum did not move on a member change (%+v)", perr)
	}
}

func TestRejectBackupTimezone(t *testing.T) {
	if perr := rejectBackupTimezone(map[string]string{}); perr != nil {
		t.Errorf("no declaration must pass: %+v", perr)
	}
	perr := rejectBackupTimezone(map[string]string{"backup_timezone": "UTC"})
	if perr == nil || perr.Code != "invalid_request" || !strings.Contains(perr.Message, "UTC by construction") {
		t.Errorf("perr = %+v", perr)
	}
}

// tarEntry is one entry of a test archive.
type tarEntry struct {
	name    string
	content string
	dir     bool
}

// buildTar writes a tar (optionally gzip) with the given entries.
func buildTar(t *testing.T, path string, gz bool, entries []tarEntry) string {
	t.Helper()
	buf := &bytes.Buffer{}
	var w *tar.Writer
	var gzw *gzip.Writer
	if gz {
		gzw = gzip.NewWriter(buf)
		w = tar.NewWriter(gzw)
	} else {
		w = tar.NewWriter(buf)
	}
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o600, Size: int64(len(e.content))}
		if e.dir {
			hdr.Typeflag = tar.TypeDir
			hdr.Mode = 0o755
		}
		if err := w.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if !e.dir {
			if _, err := w.Write([]byte(e.content)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if gzw != nil {
		if err := gzw.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// backupTarEntries tars a healthy backup set, optionally under one
// wrapping directory.
func backupTarEntries(wrap string) []tarEntry {
	prefix := ""
	if wrap != "" {
		prefix = wrap + "/"
	}
	shards := []string{stemA + ".1.tar.gz", stemA + ".2.tar.gz"}
	entries := []tarEntry{}
	if wrap != "" {
		entries = append(entries, tarEntry{name: wrap + "/", dir: true})
	}
	entries = append(entries,
		tarEntry{name: prefix + stemA + ".manifest", content: manifestJSON(stemA, shards, singleOrg())},
		tarEntry{name: prefix + stemA + ".bolt.gz", content: "kv bytes"},
		tarEntry{name: prefix + stemA + ".sqlite.gz", content: "sql bytes"},
	)
	for _, s := range shards {
		entries = append(entries, tarEntry{name: prefix + s, content: "shard bytes"})
	}
	return entries
}

func TestResolveTarLayouts(t *testing.T) {
	t.Run("a plain archive with members at the root", func(t *testing.T) {
		path := buildTar(t, filepath.Join(t.TempDir(), "bak.tar"), false, backupTarEntries(""))
		src, perr := resolveSource("influx_backup_tar", path)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		if !src.tarball || src.createdAt == nil || *src.createdAt != "2026-08-17T10:00:00.000Z" {
			t.Errorf("src = tarball %v created_at %v", src.tarball, src.createdAt)
		}
		if got := strings.Join(src.orgs["probavi-org"], "|"); got != "metrics|events" {
			t.Errorf("orgs = %+v", src.orgs)
		}
	})

	t.Run("a gzip archive with one wrapping directory", func(t *testing.T) {
		path := buildTar(t, filepath.Join(t.TempDir(), "bak.tar.gz"), true, backupTarEntries("bak-20260817"))
		src, perr := resolveSource("influx_backup_tar", path)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		if len(src.orgs) != 1 || src.createdAt == nil {
			t.Errorf("src = %+v", src)
		}
	})

}

// TestResolveTarOpaque pins the bonus-only nature of the host walk.
func TestResolveTarOpaque(t *testing.T) {
	{
		path := filepath.Join(t.TempDir(), "opaque.bin")
		if err := os.WriteFile(path, bytes.Repeat([]byte{0xA5}, 4096), 0o600); err != nil {
			t.Fatal(err)
		}
		src, perr := resolveSource("influx_backup_tar", path)
		if perr != nil {
			t.Fatalf("resolveSource: %+v — the sandbox extraction is the authority", perr)
		}
		if len(src.orgs) != 0 || src.createdAt != nil || !strings.HasPrefix(src.checksum, "sha256:") {
			t.Errorf("src = %+v, want nothing claimed and a real checksum", src)
		}
	}
}

// TestResolveTarRefusals pins the archive-side fences.
func TestResolveTarRefusals(t *testing.T) {
	t.Run("a tarred 1.x portable backup is refused host-side", func(t *testing.T) {
		path := buildTar(t, filepath.Join(t.TempDir(), "portable.tar"), false, []tarEntry{
			{name: stemA + ".manifest", content: `{"meta":{"fileName":"` + stemA + `.meta","size":66},"limited":false,"files":null}`},
			{name: stemA + ".meta", content: "meta bytes"},
		})
		_, perr := resolveSource("influx_backup_tar", path)
		if perr == nil || perr.Code != "unsupported_source" || !strings.Contains(perr.Message, "migration") {
			t.Fatalf("perr = %+v, want the migration fence", perr)
		}
	})

	t.Run("an archive missing a named member is an incomplete copy", func(t *testing.T) {
		entries := backupTarEntries("")[:4] // drop the last shard
		path := buildTar(t, filepath.Join(t.TempDir(), "short.tar"), false, entries)
		_, perr := resolveSource("influx_backup_tar", path)
		if perr == nil || perr.Code != "source_corrupt" || !strings.Contains(perr.Message, ".2.tar.gz") {
			t.Fatalf("perr = %+v, want the incomplete copy named", perr)
		}
	})

	t.Run("a directory for the tar kind teaches the directory kinds", func(t *testing.T) {
		_, perr := resolveSource("influx_backup_tar", t.TempDir())
		if perr == nil || perr.Code != "invalid_request" || !strings.Contains(perr.Message, "influx_backup_dir") {
			t.Fatalf("perr = %+v", perr)
		}
	})

}

// TestResolveTarPicksNewest mirrors the reused-directory rule for an
// archive holding several sets.
func TestResolveTarPicksNewest(t *testing.T) {
	{
		entries := append(backupTarEntries(""),
			tarEntry{name: "20260810T000000Z.manifest", content: manifestJSON("20260810T000000Z", nil, singleOrg())},
			tarEntry{name: "20260810T000000Z.bolt.gz", content: "kv"},
			tarEntry{name: "20260810T000000Z.sqlite.gz", content: "sql"})
		path := buildTar(t, filepath.Join(t.TempDir(), "two.tar"), false, entries)
		src, perr := resolveSource("influx_backup_tar", path)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		if src.manifestName != stemA+".manifest" {
			t.Errorf("chose %q, want the newest stem", src.manifestName)
		}
	}
}
