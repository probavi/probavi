package main

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tarOf packs a directory into a tar archive, optionally under one
// wrapping directory — both shapes an operator's `tar -cf` produces.
func tarOf(t *testing.T, root, prefix string) string {
	t.Helper()
	buf := &bytes.Buffer{}
	tw := tar.NewWriter(buf)
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil || rel == "." {
			return err
		}
		name := filepath.ToSlash(rel)
		if prefix != "" {
			name = prefix + "/" + name
		}
		if d.IsDir() {
			return tw.WriteHeader(&tar.Header{Name: name + "/", Typeflag: tar.TypeDir, Mode: 0o755})
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(body)),
		}); err != nil {
			return err
		}
		_, err = tw.Write(body)
		return err
	})
	if err != nil {
		t.Fatalf("pack tar: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	path := filepath.Join(t.TempDir(), "dump.tar")
	writeFile(t, path, buf.String())
	return path
}

func wantRefusal(t *testing.T, kind, path, code, phrase string) {
	t.Helper()
	_, perr := resolveSource(kind, path, nil)
	if perr == nil {
		t.Fatalf("resolveSource(%s, %s) succeeded, want %s", kind, path, code)
	}
	if perr.Code != code {
		t.Errorf("code = %q, want %q (message: %s)", perr.Code, code, perr.Message)
	}
	if !strings.Contains(perr.Message, phrase) {
		t.Errorf("message = %q, want it to mention %q", perr.Message, phrase)
	}
}

func TestADumpIsReadForWhatItStatesAboutItself(t *testing.T) {
	dir := t.TempDir()
	writeDump(t, dir, oneDump())
	src, perr := resolveSource("arangodb_dump", dir, nil)
	if perr != nil {
		t.Fatalf("resolveSource: %+v", perr)
	}
	if src.census.database != "shop" {
		t.Errorf("database = %q, want the one dump.json names", src.census.database)
	}
	if len(src.census.collections) != 2 || src.census.collections[0] != "meta" {
		t.Errorf("collections = %v, want both, sorted", src.census.collections)
	}
	if src.census.createdMs == 0 {
		t.Error("the dump's own instant was not read")
	}
	if src.tarball || src.sizeBytes <= 0 || !strings.HasPrefix(src.checksum, "sha256:") {
		t.Errorf("identity = %q/%d tar=%v", src.checksum, src.sizeBytes, src.tarball)
	}
}

func TestAnArchiveIsWalkedOnTheHost(t *testing.T) {
	for name, prefix := range map[string]string{
		"files at the root":            "",
		"under one wrapping directory": "backup-2026-09-20",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeDump(t, dir, oneDump())
			src, perr := resolveSource("arangodb_dump_tar", tarOf(t, dir, prefix), nil)
			if perr != nil {
				t.Fatalf("resolveSource: %+v", perr)
			}
			if !src.tarball {
				t.Error("an archive was not resolved as one")
			}
			if src.census.database != "shop" || len(src.census.collections) != 2 {
				t.Errorf("census = %+v, want it read out of the archive", src.census)
			}
		})
	}
}

// TestAStreamThatIsNotTarShapedSaysNothing: the host's pass is a bonus.
// Where it cannot read the stream it must not produce a verdict — the
// sandbox's own tar is then the authority.
func TestAStreamThatIsNotTarShapedSaysNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notatar.tar")
	writeFile(t, path, "this is not a tar archive at all\n")
	src, perr := resolveSource("arangodb_dump_tar", path, nil)
	if perr != nil {
		t.Fatalf("resolveSource refused an unreadable stream: %+v", perr)
	}
	if src.census.database != "" || len(src.census.collections) != 0 {
		t.Errorf("census = %+v, want nothing claimed about a stream that was not read", src.census)
	}
}

// TestEveryCollectionNeedsBothHalves is the completeness gate. dump.json
// carries no list of collections, so the check is a pairing: arangodump
// writes a structure and a data file for every collection, including one
// with no documents (measured), which is what makes this safe.
func TestEveryCollectionNeedsBothHalves(t *testing.T) {
	t.Run("data file missing", func(t *testing.T) {
		dir := t.TempDir()
		f := oneDump()
		f.omitData = true
		writeDump(t, dir, f)
		wantRefusal(t, "arangodb_dump", dir, "source_corrupt", "definition and no data file")
	})
	t.Run("definition missing", func(t *testing.T) {
		dir := t.TempDir()
		f := oneDump()
		f.omitStructure = true
		writeDump(t, dir, f)
		wantRefusal(t, "arangodb_dump", dir, "source_corrupt", "data file and no definition")
	})
	t.Run("no collection at all", func(t *testing.T) {
		dir := t.TempDir()
		writeDump(t, dir, dumpFixture{database: "shop", createdAt: "2026-09-20T14:21:39Z"})
		wantRefusal(t, "arangodb_dump", dir, "source_corrupt", "carries no collection at all")
	})
	t.Run("through the archive path", func(t *testing.T) {
		dir := t.TempDir()
		f := oneDump()
		f.omitData = true
		writeDump(t, dir, f)
		wantRefusal(t, "arangodb_dump_tar", tarOf(t, dir, ""), "source_corrupt", "no data file")
	})
}

// TestAnEncryptedDumpIsRefusedByName: encryption is an Enterprise
// feature and the marker file is written either way, so the artifact says
// so itself rather than failing obscurely later.
func TestAnEncryptedDumpIsRefusedByName(t *testing.T) {
	dir := t.TempDir()
	f := oneDump()
	f.encryption = "aes-256-ctr"
	writeDump(t, dir, f)
	wantRefusal(t, "arangodb_dump", dir, "unsupported_source", "states encryption")
}

func TestWhatADumpMustCarry(t *testing.T) {
	t.Run("no manifest", func(t *testing.T) {
		dir := t.TempDir()
		f := oneDump()
		f.omitManifest = true
		writeDump(t, dir, f)
		wantRefusal(t, "arangodb_dump", dir, "source_corrupt", "holds no readable dump.json")
	})
	t.Run("a manifest naming no database", func(t *testing.T) {
		dir := t.TempDir()
		f := oneDump()
		f.database = ""
		writeDump(t, dir, f)
		wantRefusal(t, "arangodb_dump", dir, "source_corrupt", "names no database")
	})
	t.Run("a database name that cannot be used", func(t *testing.T) {
		dir := t.TempDir()
		f := oneDump()
		f.database = "1shop"
		writeDump(t, dir, f)
		wantRefusal(t, "arangodb_dump", dir, "invalid_request", "not one this adapter can put into")
	})
	t.Run("a manifest that does not parse", func(t *testing.T) {
		dir := t.TempDir()
		writeDump(t, dir, dumpFixture{omitManifest: true, collections: []string{"orders"}})
		writeFile(t, filepath.Join(dir, manifestName), "{not json")
		wantRefusal(t, "arangodb_dump", dir, "source_corrupt", "holds no readable dump.json")
	})
}

func TestTheDirectoryKindPicksWhatTheManifestsDate(t *testing.T) {
	root := t.TempDir()
	for name, created := range map[string]string{
		"monday":  "2026-09-14T03:00:00Z",
		"tuesday": "2026-09-19T03:00:00Z",
		"sunday":  "2026-09-06T03:00:00Z",
	} {
		sub := filepath.Join(root, name)
		f := oneDump()
		f.createdAt = created
		writeDump(t, sub, f)
		// File times run the other way, so a ranking by mtime would pick
		// the wrong one — which is the mistake being avoided.
		old := time.Now().Add(-time.Duration(len(name)) * time.Hour)
		if err := os.Chtimes(sub, old, old); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}
	src, perr := resolveSource("arangodb_dump_dir", root, nil)
	if perr != nil {
		t.Fatalf("resolveSource: %+v", perr)
	}
	if filepath.Base(src.path) != "tuesday" {
		t.Errorf("chose %q, want the dump whose own manifest claims the newest instant", src.path)
	}
}

func TestADatedDumpOutranksAnUndatedOne(t *testing.T) {
	dated := dumpCandidate{name: "a", createdMs: 1}
	undated := dumpCandidate{name: "z", mtime: time.Now()}
	if !dated.beats(undated) || undated.beats(dated) {
		t.Error("a dated dump must outrank an undated one, and not the other way")
	}
	older := dumpCandidate{name: "b", mtime: time.Unix(1000, 0)}
	newer := dumpCandidate{name: "a", mtime: time.Unix(2000, 0)}
	if !newer.beats(older) {
		t.Error("undated candidates did not fall back to directory time")
	}
	if !older.beats(dumpCandidate{name: "a", mtime: older.mtime}) {
		t.Error("the name tiebreak is not deterministic")
	}
}

func TestTheKindsAndWhatTheyRefuse(t *testing.T) {
	dir := t.TempDir()
	writeDump(t, dir, oneDump())
	file := tarOf(t, dir, "")

	wantRefusal(t, "arangodb_dump", file, "invalid_request", "is a file")
	wantRefusal(t, "arangodb_dump_tar", dir, "invalid_request", "is a directory")
	wantRefusal(t, "arangodb_dump", filepath.Join(dir, "nope"), "source_not_found", "does not exist")
	wantRefusal(t, "arangodb_dump_tar", filepath.Join(dir, "nope.tar"), "source_not_found", "does not exist")
	wantRefusal(t, "arangodb_dump_dir", filepath.Join(dir, "nope"), "source_not_found", "does not exist")
	wantRefusal(t, "mysqldump", file, "unsupported_source", "unsupported source kind")
	wantRefusal(t, "arangodb_dump_dir", t.TempDir(), "source_not_found", "contains no dumps")
}

// TestADeclaredTimezoneIsRefusedRatherThanIgnored: dump.json states an
// instant in UTC, so the parameter cannot improve anything.
func TestADeclaredTimezoneIsRefusedRatherThanIgnored(t *testing.T) {
	if perr := rejectBackupTimezone(nil); perr != nil {
		t.Errorf("no parameter was refused: %+v", perr)
	}
	perr := rejectBackupTimezone(map[string]string{backupTimezoneParam: "Europe/Budapest"})
	if perr == nil || perr.Code != "invalid_request" {
		t.Fatalf("a declared zone was accepted: %+v", perr)
	}
	if !strings.Contains(perr.Message, "states its instant in UTC") {
		t.Errorf("message = %q, want it to say why the parameter is redundant", perr.Message)
	}
}

func TestAnInstantOutsideTheRangeDoesNotDateARecord(t *testing.T) {
	for name, tc := range map[string]struct {
		createdAt string
		want      bool
	}{
		"a real instant":    {"2026-09-20T14:21:39Z", true},
		"empty":             {"", false},
		"before the engine": {"1998-01-01T00:00:00Z", false},
		"far in the future": {"2999-01-01T00:00:00Z", false},
		"not a timestamp":   {"yesterday", false},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			f := oneDump()
			f.createdAt = tc.createdAt
			writeDump(t, dir, f)
			src, perr := resolveSource("arangodb_dump", dir, nil)
			if perr != nil {
				t.Fatalf("resolveSource: %+v", perr)
			}
			if got := src.census.createdMs != 0; got != tc.want {
				t.Errorf("dated = %v, want %v for createdAt %q", got, tc.want, tc.createdAt)
			}
		})
	}
}

func TestFormatCreatedAtRendersUTCMilliseconds(t *testing.T) {
	if got := formatCreatedAt(0); got != nil {
		t.Errorf("formatCreatedAt(0) = %v, want nil", *got)
	}
	got := formatCreatedAt(1789900285123)
	if got == nil || !strings.HasSuffix(*got, "Z") || !strings.Contains(*got, ".123") {
		t.Errorf("created_at = %v, want UTC with milliseconds", got)
	}
}

func TestADirectoryTheHostCannotReadIsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every directory")
	}
	dir := t.TempDir()
	writeDump(t, dir, oneDump())
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Errorf("restore permissions: %v", err)
		}
	})
	_, perr := resolveSource("arangodb_dump", dir, nil)
	if perr == nil || perr.Code != "source_unreadable" {
		t.Errorf("verdict = %+v, want source_unreadable", perr)
	}
}

func TestFileChecksumRefusesWhatItCannotRead(t *testing.T) {
	if _, perr := fileChecksum(filepath.Join(t.TempDir(), "absent")); perr == nil ||
		perr.Code != "source_unreadable" {
		t.Errorf("fileChecksum of a missing file = %+v, want source_unreadable", perr)
	}
}

// TestTheDumpFileNamesAreTheOnesTheToolWrites pins the shape against a
// real arangodump output, so a rename upstream fails here rather than
// silently pairing nothing.
func TestTheDumpFileNamesAreTheOnesTheToolWrites(t *testing.T) {
	for name, want := range map[string]string{
		"orders_12c500ed0b7879105fb46af0f246be87.structure.json":           "orders",
		"orders_12c500ed0b7879105fb46af0f246be87.data.json.gz":             "orders",
		"meta_e9a23cbc455158951716b440c3d165e0.data.json":                  "meta",
		"with_underscores_e9a23cbc455158951716b440c3d165e0.structure.json": "with_underscores",
	} {
		m := collectionShape.FindStringSubmatch(name)
		if m == nil {
			t.Errorf("%q did not match the shape arangodump writes", name)
			continue
		}
		if m[1] != want {
			t.Errorf("%q named collection %q, want %q", name, m[1], want)
		}
	}
	for _, name := range []string{"dump.json", "ENCRYPTION", "orders.structure.json", "orders_short.data.json.gz"} {
		if collectionShape.MatchString(name) {
			t.Errorf("%q matched the collection shape and should not", name)
		}
	}
}
