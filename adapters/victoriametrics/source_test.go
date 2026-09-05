package main

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// partFile is the name vmbackup gives the single file inside a part
// directory: <size>_<offset>_<size>, all hex (measured).
const partFile = "0000000000000027_0000000000000000_0000000000000027"

// writeBackup lays out a vmbackup output the way the tool writes one: the
// two markers at the root, and per partition a parts.json directory
// naming the parts that partition holds, each of which is itself a
// directory of part files.
func writeBackup(t *testing.T, dir, createdAt string, partitions map[string][]string) string {
	t.Helper()
	writeAt(t, filepath.Join(dir, completeMarker), "")
	if createdAt != "" {
		writeAt(t, filepath.Join(dir, metadataMarker),
			fmt.Sprintf(`{"created_at":%q,"completed_at":%q}`, createdAt, createdAt))
	}
	for partition, parts := range partitions {
		pdir := filepath.Join(dir, filepath.FromSlash(partition))
		writeAt(t, filepath.Join(pdir, partsName, partFile), partsJSON(parts))
		for _, name := range parts {
			writeAt(t, filepath.Join(pdir, name, "values.bin", partFile), "payload-"+name)
		}
	}
	return dir
}

func partsJSON(parts []string) string {
	quoted := make([]string, 0, len(parts))
	for _, p := range parts {
		quoted = append(quoted, fmt.Sprintf("%q", p))
	}
	return `{"Small":[` + strings.Join(quoted, ",") + `],"Big":[]}`
}

func writeAt(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// onePartition is the smallest healthy artifact these tests use.
func onePartition() map[string][]string {
	return map[string][]string{"data/small/2026_08": {"18CCF994FD3EEE18"}}
}

func TestResolveBackupDir(t *testing.T) {
	dir := writeBackup(t, filepath.Join(t.TempDir(), "backup"), "2026-08-18T18:23:25Z", onePartition())
	src, perr := resolveSource("victoriametrics_backup", dir)
	if perr != nil {
		t.Fatalf("resolveSource: %+v", perr)
	}
	if got := formatCreatedAt(src.info.createdAtMs); got == nil || *got != "2026-08-18T18:23:25.000Z" {
		t.Errorf("created_at = %v, want the instant the backup states", got)
	}
	if src.info.parts != 1 {
		t.Errorf("parts = %d, want the one the partition declares", src.info.parts)
	}
	if !strings.HasPrefix(src.checksum, "sha256:") || src.sizeBytes == 0 || src.tarball {
		t.Errorf("source = %+v", src)
	}
}

// TestResolveBackupDirRefusals walks the fences a directory artifact
// faces, each one a measured failure mode rather than a guess.
func TestResolveBackupDirRefusals(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(t *testing.T, dir string)
		wantCode string
		wantMsg  string
	}{
		{
			name: "a copy of a live storage path carries the server's lock",
			mutate: func(t *testing.T, dir string) {
				writeAt(t, filepath.Join(dir, "flock.lock"), "")
			},
			wantCode: "unsupported_source", wantMsg: "flock.lock",
		},
		{
			name: "a copy of a live storage path carries its snapshots",
			mutate: func(t *testing.T, dir string) {
				writeAt(t, filepath.Join(dir, "snapshots", "20260818", "keep"), "")
			},
			wantCode: "unsupported_source", wantMsg: "snapshots",
		},
		{
			name: "a copy of a live storage path carries its scratch directory",
			mutate: func(t *testing.T, dir string) {
				writeAt(t, filepath.Join(dir, "tmp", "keep"), "")
			},
			wantCode: "unsupported_source", wantMsg: "tmp",
		},
		{
			name: "a backup that never finished has no completion marker",
			mutate: func(t *testing.T, dir string) {
				if err := os.Remove(filepath.Join(dir, completeMarker)); err != nil {
					t.Fatal(err)
				}
			},
			wantCode: "source_corrupt", wantMsg: completeMarker,
		},
		{
			name: "a tree that is not a backup at all",
			mutate: func(t *testing.T, dir string) {
				if err := os.Remove(filepath.Join(dir, metadataMarker)); err != nil {
					t.Fatal(err)
				}
			},
			wantCode: "source_corrupt", wantMsg: metadataMarker,
		},
		{
			name: "metadata that states no instant",
			mutate: func(t *testing.T, dir string) {
				writeAt(t, filepath.Join(dir, metadataMarker), `{"completed_at":"2026-08-18T18:23:55Z"}`)
			},
			wantCode: "source_corrupt", wantMsg: "created_at",
		},
		{
			name: "a truncated copy misses a part its own parts.json names",
			mutate: func(t *testing.T, dir string) {
				if err := os.RemoveAll(filepath.Join(dir, "data", "small", "2026_08",
					"18CCF994FD3EEE18")); err != nil {
					t.Fatal(err)
				}
			},
			wantCode: "source_corrupt", wantMsg: "18CCF994FD3EEE18",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeBackup(t, filepath.Join(t.TempDir(), "backup"),
				"2026-08-18T18:23:25Z", onePartition())
			tc.mutate(t, dir)
			_, perr := resolveSource("victoriametrics_backup", dir)
			if perr == nil {
				t.Fatal("artifact accepted, want a refusal")
			}
			if perr.Code != tc.wantCode || !strings.Contains(perr.Message, tc.wantMsg) {
				t.Errorf("refusal = %s %q, want %s containing %q",
					perr.Code, perr.Message, tc.wantCode, tc.wantMsg)
			}
		})
	}
}

func TestResolveSourceEdges(t *testing.T) {
	dir := writeBackup(t, filepath.Join(t.TempDir(), "backup"), "2026-08-18T18:23:25Z", onePartition())
	file := filepath.Join(t.TempDir(), "backup.tar")
	writeAt(t, file, "not really a tar")

	tests := []struct {
		name, kind, path, wantCode string
	}{
		{"an unknown kind", "victoriametrics_dump", dir, "unsupported_source"},
		{"a directory given to the archive kind", "victoriametrics_backup_tar", dir, "invalid_request"},
		{"a file given to the directory kind", "victoriametrics_backup", file, "invalid_request"},
		{"a path that does not exist", "victoriametrics_backup",
			filepath.Join(t.TempDir(), "absent"), "source_not_found"},
		{"an archive path that does not exist", "victoriametrics_backup_tar",
			filepath.Join(t.TempDir(), "absent.tar"), "source_not_found"},
		{"a directory of backups that does not exist", "victoriametrics_backup_dir",
			filepath.Join(t.TempDir(), "absent"), "source_not_found"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, perr := resolveSource(tc.kind, tc.path)
			if perr == nil || perr.Code != tc.wantCode {
				t.Errorf("refusal = %+v, want %s", perr, tc.wantCode)
			}
		})
	}
}

// TestNewestBackupIn pins the ranking: the backup that can date itself
// wins, and it wins by what it says rather than by when it was copied.
func TestNewestBackupIn(t *testing.T) {
	root := t.TempDir()
	writeBackup(t, filepath.Join(root, "a-oldest"), "2026-08-01T00:00:00Z", onePartition())
	writeBackup(t, filepath.Join(root, "b-newest"), "2026-08-18T18:23:25Z", onePartition())
	writeBackup(t, filepath.Join(root, "c-undated"), "", onePartition())

	src, perr := resolveSource("victoriametrics_backup_dir", root)
	if perr != nil {
		t.Fatalf("resolveSource: %+v", perr)
	}
	if filepath.Base(src.path) != "b-newest" {
		t.Errorf("restored %s, want the backup claiming the newest instant", filepath.Base(src.path))
	}

	empty := t.TempDir()
	if _, perr := resolveSource("victoriametrics_backup_dir", empty); perr == nil ||
		perr.Code != "source_not_found" {
		t.Errorf("refusal = %+v, want source_not_found for a directory holding no backups", perr)
	}
}

// TestNewestBackupInRefusesTheBrokenWinner proves the ranking does not
// launder a bad artifact: the winner still faces every fence.
func TestNewestBackupInRefusesTheBrokenWinner(t *testing.T) {
	root := t.TempDir()
	writeBackup(t, filepath.Join(root, "healthy"), "2026-08-01T00:00:00Z", onePartition())
	winner := writeBackup(t, filepath.Join(root, "winner"), "2026-08-18T18:23:25Z", onePartition())
	writeAt(t, filepath.Join(winner, "flock.lock"), "")

	_, perr := resolveSource("victoriametrics_backup_dir", root)
	if perr == nil || perr.Code != "unsupported_source" {
		t.Fatalf("refusal = %+v, want the live-copy refusal", perr)
	}
}

// buildTar writes an archive of a directory tree, optionally gzipped,
// with every path prefixed by wrap when it is not empty — the wrapping
// directory `tar -czf x.tar.gz backup` naturally produces.
func buildTar(t *testing.T, dest, root, wrap string, gzipped bool) string {
	t.Helper()
	f, err := os.Create(dest)
	if err != nil {
		t.Fatal(err)
	}
	closers := []io.Closer{f}
	var sink io.Writer = f
	if gzipped {
		gz := gzip.NewWriter(f)
		closers = append([]io.Closer{gz}, closers...)
		sink = gz
	}
	tw := tar.NewWriter(sink)
	closers = append([]io.Closer{tw}, closers...)

	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil || rel == "." {
			return err
		}
		return writeTarEntry(tw, p, tarEntryName(wrap, rel), d)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range closers {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return dest
}

// tarEntryName renders one archive path, under the wrapping directory
// when the caller asked for one.
func tarEntryName(wrap, rel string) string {
	name := filepath.ToSlash(rel)
	if wrap == "" {
		return name
	}
	return wrap + "/" + name
}

func writeTarEntry(tw *tar.Writer, path, name string, d os.DirEntry) error {
	if d.IsDir() {
		return tw.WriteHeader(&tar.Header{Name: name + "/", Mode: 0o755, Typeflag: tar.TypeDir})
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: name, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err = tw.Write(body)
	return err
}

func TestResolveTar(t *testing.T) {
	backup := writeBackup(t, filepath.Join(t.TempDir(), "backup"), "2026-08-18T18:23:25Z", onePartition())
	for _, tc := range []struct {
		name    string
		wrap    string
		gzipped bool
	}{
		{"files at the archive root", "", false},
		{"under one wrapping directory", "backup", false},
		{"gzip compressed", "backup", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			archive := buildTar(t, filepath.Join(t.TempDir(), "b.tar"), backup, tc.wrap, tc.gzipped)
			src, perr := resolveSource("victoriametrics_backup_tar", archive)
			if perr != nil {
				t.Fatalf("resolveSource: %+v", perr)
			}
			if !src.tarball || !strings.HasPrefix(src.checksum, "sha256:") {
				t.Errorf("source = %+v", src)
			}
			if got := formatCreatedAt(src.info.createdAtMs); got == nil ||
				*got != "2026-08-18T18:23:25.000Z" {
				t.Errorf("created_at = %v, want the instant the archive states", got)
			}
		})
	}
}

func TestResolveTarRefusals(t *testing.T) {
	t.Run("an archive of a live storage path", func(t *testing.T) {
		live := writeBackup(t, filepath.Join(t.TempDir(), "live"), "2026-08-18T18:23:25Z", onePartition())
		writeAt(t, filepath.Join(live, "flock.lock"), "")
		archive := buildTar(t, filepath.Join(t.TempDir(), "live.tar"), live, "", false)
		_, perr := resolveSource("victoriametrics_backup_tar", archive)
		if perr == nil || perr.Code != "unsupported_source" ||
			!strings.Contains(perr.Message, "flock.lock") {
			t.Errorf("refusal = %+v, want the live-copy refusal naming the lock", perr)
		}
	})

	t.Run("an archive holding no backup at all", func(t *testing.T) {
		other := t.TempDir()
		writeAt(t, filepath.Join(other, "notes.txt"), "hello")
		archive := buildTar(t, filepath.Join(t.TempDir(), "other.tar"), other, "", false)
		_, perr := resolveSource("victoriametrics_backup_tar", archive)
		if perr == nil || perr.Code != "source_corrupt" {
			t.Errorf("refusal = %+v, want source_corrupt", perr)
		}
	})

	t.Run("an archive whose backup never finished", func(t *testing.T) {
		backup := writeBackup(t, filepath.Join(t.TempDir(), "backup"),
			"2026-08-18T18:23:25Z", onePartition())
		if err := os.Remove(filepath.Join(backup, completeMarker)); err != nil {
			t.Fatal(err)
		}
		archive := buildTar(t, filepath.Join(t.TempDir(), "partial.tar"), backup, "", false)
		_, perr := resolveSource("victoriametrics_backup_tar", archive)
		if perr == nil || perr.Code != "source_corrupt" ||
			!strings.Contains(perr.Message, completeMarker) {
			t.Errorf("refusal = %+v, want the incomplete-backup refusal", perr)
		}
	})

	// An archive nothing could walk is not evidence of anything: it
	// resolves, and the sandbox extraction becomes the authority.
	t.Run("an archive the host cannot walk", func(t *testing.T) {
		opaque := filepath.Join(t.TempDir(), "opaque.tar")
		writeAt(t, opaque, "\x1f\x8bnot really a gzip stream")
		src, perr := resolveSource("victoriametrics_backup_tar", opaque)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		if src.info.createdAtMs != 0 {
			t.Errorf("info = %+v, want the zero value the sandbox then recovers", src.info)
		}
	})
}

func TestParseBackupMetadata(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{"the shape vmbackup writes", `{"created_at":"2026-08-18T18:23:25Z","completed_at":"2026-08-18T18:23:55Z"}`, true},
		{"an offset instead of Z", `{"created_at":"2026-08-18T20:23:25+02:00"}`, true},
		{"no created_at", `{"completed_at":"2026-08-18T18:23:55Z"}`, false},
		{"an unparseable instant", `{"created_at":"yesterday"}`, false},
		{"not JSON at all", `nope`, false},
		// Found by FuzzParseBackupMetadata. The artifact's own created_at
		// picks the backup to restore, dates the record, and is the instant
		// the series census evaluates at, so a value no snapshot was taken
		// at must not be read as one. The epoch is the sharpest case: every
		// caller spells "this artifact does not date itself" as zero.
		{"the epoch itself is how a caller says 'undated'", `{"created_at":"1970-01-01T00:00:00Z"}`, false},
		{"an instant no snapshot was taken at", `{"created_at":"1899-12-31T23:59:59Z"}`, false},
		{"an instant no snapshot is written ahead to", `{"created_at":"9999-01-01T00:00:00Z"}`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ms, ok := parseBackupMetadata([]byte(tc.raw))
			if ok != tc.want {
				t.Fatalf("ok = %v, want %v", ok, tc.want)
			}
			if ok && ms == 0 {
				t.Error("parsed instant is zero")
			}
		})
	}
}

func TestDeclaredParts(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want int
	}{
		{"the data partition's object", `{"Small":["a","b"],"Big":["c"]}`, 3},
		{"an index partition's array", `["a"]`, 1},
		{"an empty array", `[]`, 0},
		{"a shape neither reader knows", `42`, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := len(declaredParts([]byte(tc.raw))); got != tc.want {
				t.Errorf("declaredParts = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestPartDirExistsStaysInsideThePartition covers the completeness fence
// against the artifact that would talk past it. parts.json is the backup's
// own statement of what a partition requires, and the census refuses a
// backup missing what it names — so a name resolving anywhere but inside
// the partition would let a truncated copy pass by pointing at a directory
// the backup does not contain. Found by FuzzDeclaredParts.
func TestPartDirExistsStaysInsideThePartition(t *testing.T) {
	root := t.TempDir()
	partition := filepath.Join(root, "partition")
	if err := os.MkdirAll(filepath.Join(partition, "1_2_3_ABC"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "elsewhere"), 0o750); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{
		"1_2_3_ABC":              true,
		"../elsewhere":           false,
		"../partition/1_2_3_ABC": false,
		".":                      false,
		"..":                     false,
		"":                       false,
		"1_2_3_ABC/../1_2_3_ABC": false,
	} {
		if got := partDirExists(partition, name); got != want {
			t.Errorf("partDirExists(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestFormatCreatedAt(t *testing.T) {
	if got := formatCreatedAt(0); got != nil {
		t.Errorf("formatCreatedAt(0) = %v, want nil", got)
	}
	if got := formatCreatedAt(1787077405000); got == nil || !strings.HasSuffix(*got, "Z") {
		t.Errorf("formatCreatedAt = %v, want a UTC instant", got)
	}
}

func TestRejectBackupTimezone(t *testing.T) {
	if perr := rejectBackupTimezone(nil); perr != nil {
		t.Errorf("no declaration must pass: %+v", perr)
	}
	perr := rejectBackupTimezone(map[string]string{backupTimezoneParam: "Europe/Budapest"})
	if perr == nil || perr.Code != "invalid_request" ||
		!strings.Contains(perr.Message, backupTimezoneParam) {
		t.Errorf("refusal = %+v, want invalid_request naming the parameter", perr)
	}
}
