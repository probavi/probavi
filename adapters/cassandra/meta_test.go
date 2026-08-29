package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureCreatedAt is the manifest instant the fixtures claim, with its
// epoch-millisecond value beside it (2026-08-16T11:35:49.263Z, the
// measured 5.0 shape).
const (
	fixtureCreatedAt = "2026-08-16T11:35:49.263Z"
	fixtureCreatedMs = int64(1786880149263)
)

// tableFixture controls what a fixture table directory claims.
type tableFixture struct {
	createdAt     string
	dropComponent string // omit this component file while TOC still lists it
	corruptData   bool   // change Data.db bytes after the digest was written
	noSchema      bool
	noManifest    bool
	liveMarker    string // add this subdirectory
}

// sstableComponents is the measured nb-generation component set.
var sstableComponents = []string{
	"CompressionInfo.db", "Data.db", "Digest.crc32", "Filter.db",
	"Index.db", "Statistics.db", "Summary.db", "TOC.txt",
}

// writeTable lays down one snapshot table directory making the claims the
// fixture asks for.
func writeTable(t *testing.T, root, keyspace, table string, fx tableFixture) {
	t.Helper()
	dir := filepath.Join(root, keyspace, table)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte("sstable data bytes for " + keyspace + "." + table)
	digest := crc32.ChecksumIEEE(data)
	if fx.corruptData {
		data = append([]byte("CORRUPTED "), data...)
	}
	createdAt := fx.createdAt
	if createdAt == "" {
		createdAt = fixtureCreatedAt
	}
	files := map[string][]byte{
		"nb-1-big-Data.db":            data,
		"nb-1-big-Index.db":           []byte("index"),
		"nb-1-big-Filter.db":          []byte("filter"),
		"nb-1-big-Statistics.db":      []byte("stats"),
		"nb-1-big-Summary.db":         []byte("summary"),
		"nb-1-big-CompressionInfo.db": []byte("compression"),
		"nb-1-big-Digest.crc32":       []byte(fmt.Sprintf("%d", digest)),
		"nb-1-big-TOC.txt":            []byte(strings.Join(sstableComponents, "\n") + "\n"),
	}
	if !fx.noSchema {
		files["schema.cql"] = []byte(fmt.Sprintf(
			"CREATE TABLE IF NOT EXISTS %s.%s (\n    id int PRIMARY KEY,\n    v text\n);\n",
			keyspace, table))
	}
	if !fx.noManifest {
		files["manifest.json"] = []byte(fmt.Sprintf(
			`{"files":["nb-1-big-Data.db"],"created_at":%q,"expires_at":null}`, createdAt))
	}
	if fx.dropComponent != "" {
		delete(files, "nb-1-big-"+fx.dropComponent)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if fx.liveMarker != "" {
		if err := os.MkdirAll(filepath.Join(dir, fx.liveMarker), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// writeTree lays down a healthy collected snapshot tree with the given
// keyspace.table names.
func writeTree(t *testing.T, root string, tables ...string) string {
	t.Helper()
	for _, qualified := range tables {
		parts := strings.SplitN(qualified, ".", 2)
		writeTable(t, root, parts[0], parts[1], tableFixture{})
	}
	return root
}

// treeToTar tars a tree, optionally gzip and optionally under one
// wrapping directory — the two layouts a collected snapshot is stored in.
func treeToTar(t *testing.T, root, dest, wrap string, gz bool) string {
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
	if err := filepath.WalkDir(root, tarEntryWriter(w, root, wrap)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if gzw != nil {
		if err := gzw.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(dest, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return dest
}

func tarEntryWriter(w *tar.Writer, root, wrap string) func(string, os.DirEntry, error) error {
	return func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil || rel == "." {
			return err
		}
		name := filepath.ToSlash(rel)
		if wrap != "" {
			name = wrap + "/" + name
		}
		if d.IsDir() {
			return w.WriteHeader(&tar.Header{Name: name + "/", Mode: 0o755, Typeflag: tar.TypeDir})
		}
		content, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := w.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(content))}); err != nil {
			return err
		}
		_, err = w.Write(content)
		return err
	}
}

func TestInspectSnapshotTree(t *testing.T) {
	t.Run("a healthy tree counts its tables and claims its instant", func(t *testing.T) {
		root := writeTree(t, t.TempDir(), "probavi.orders", "probavi.meta")
		// A stray sidecar at any level is not the snapshot's problem.
		if err := os.WriteFile(filepath.Join(root, "SHA256SUMS"), []byte("sums\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		census, perr := inspectSnapshotTree(root)
		if perr != nil {
			t.Fatalf("inspectSnapshotTree: %+v", perr)
		}
		if len(census.tables) != 2 || census.maxCreatedMs != fixtureCreatedMs {
			t.Errorf("census = %+v, want 2 tables claiming %d", census, fixtureCreatedMs)
		}
		if census.tables[0].String() != "probavi.meta" || census.tables[1].String() != "probavi.orders" {
			t.Errorf("tables = %v, want sorted", census.tables)
		}
	})

	t.Run("an empty directory is not a snapshot", func(t *testing.T) {
		_, perr := inspectSnapshotTree(t.TempDir())
		if perr == nil || perr.Code != "source_corrupt" || !strings.Contains(perr.Message, "snapshots/<tag>") {
			t.Fatalf("perr = %+v, want source_corrupt teaching the collection loop", perr)
		}
	})

	t.Run("a system keyspace is refused by name", func(t *testing.T) {
		root := t.TempDir()
		writeTable(t, root, "probavi", "orders", tableFixture{})
		writeTable(t, root, "system_schema", "tables", tableFixture{})
		_, perr := inspectSnapshotTree(root)
		if perr == nil || perr.Code != "invalid_request" || !strings.Contains(perr.Message, "system_schema") {
			t.Fatalf("perr = %+v, want invalid_request naming the system keyspace", perr)
		}
	})
}

func TestJudgeTableRefusals(t *testing.T) {
	tests := []struct {
		name     string
		fixture  tableFixture
		wantCode string
		wantWord string
	}{
		{"a snapshots subdirectory is a raw copy",
			tableFixture{liveMarker: "snapshots"}, "unsupported_source", "nodetool snapshot"},
		{"a backups subdirectory is a raw copy",
			tableFixture{liveMarker: "backups"}, "unsupported_source", "backups"},
		{"a table without schema.cql cannot be recreated",
			tableFixture{noSchema: true}, "source_corrupt", "schema.cql"},
		{"a component the TOC lists is missing",
			tableFixture{dropComponent: "Index.db"}, "source_corrupt", "Index.db"},
		{"a Data file contradicting its own digest",
			tableFixture{corruptData: true}, "source_corrupt", "Digest.crc32"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeTable(t, root, "probavi", "orders", tc.fixture)
			_, perr := inspectSnapshotTree(root)
			if perr == nil || perr.Code != tc.wantCode {
				t.Fatalf("perr = %+v, want %s", perr, tc.wantCode)
			}
			if !strings.Contains(perr.Message, tc.wantWord) {
				t.Errorf("message %q does not carry %q", perr.Message, tc.wantWord)
			}
		})
	}

	t.Run("a table making no claims passes here", func(t *testing.T) {
		// No TOC, no digest, no manifest: nothing to judge against — the
		// engine speaks later.
		root := t.TempDir()
		dir := filepath.Join(root, "probavi", "orders")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, content := range map[string]string{
			"schema.cql":       "CREATE TABLE IF NOT EXISTS probavi.orders (id int PRIMARY KEY);",
			"nb-1-big-Data.db": "data",
		} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		census, perr := inspectSnapshotTree(root)
		if perr != nil {
			t.Fatalf("inspectSnapshotTree: %+v", perr)
		}
		if len(census.tables) != 1 || census.maxCreatedMs != 0 {
			t.Errorf("census = %+v, want one undated table", census)
		}
	})
}

func TestJudgeName(t *testing.T) {
	for _, bad := range []string{"system", "system_auth", "Orders", "or-ders", "1orders", "_x",
		strings.Repeat("a", 49), ""} {
		if perr := judgeName("keyspace", bad); perr == nil {
			t.Errorf("judgeName(%q) = nil, want a refusal", bad)
		}
	}
	for _, good := range []string{"probavi", "orders_v2", "a", strings.Repeat("a", 48)} {
		if perr := judgeName("keyspace", good); perr != nil {
			t.Errorf("judgeName(%q) = %+v, want nil", good, perr)
		}
	}
}

func TestInspectSnapshotTar(t *testing.T) {
	t.Run("a plain archive with keyspaces at the root", func(t *testing.T) {
		root := writeTree(t, t.TempDir(), "probavi.orders", "probavi.meta")
		path := treeToTar(t, root, filepath.Join(t.TempDir(), "snap.tar"), "", false)
		census, verdict, ok := inspectSnapshotTar(path)
		if !ok || verdict != nil || len(census.tables) != 2 || census.maxCreatedMs != fixtureCreatedMs {
			t.Errorf("census = %+v verdict=%+v ok=%v", census, verdict, ok)
		}
	})

	t.Run("a gzip archive under one wrapping directory", func(t *testing.T) {
		root := writeTree(t, t.TempDir(), "probavi.orders")
		path := treeToTar(t, root, filepath.Join(t.TempDir(), "snap.tar.gz"), "snap-2026-08-16", true)
		census, verdict, ok := inspectSnapshotTar(path)
		if !ok || verdict != nil || len(census.tables) != 1 {
			t.Errorf("census = %+v verdict=%+v ok=%v", census, verdict, ok)
		}
	})

	t.Run("a tar of a raw data-directory copy is refused by its markers", func(t *testing.T) {
		root := t.TempDir()
		writeTable(t, root, "probavi", "orders", tableFixture{liveMarker: "snapshots"})
		path := treeToTar(t, root, filepath.Join(t.TempDir(), "datadir.tar"), "", false)
		_, verdict, ok := inspectSnapshotTar(path)
		if !ok || verdict == nil || verdict.Code != "unsupported_source" {
			t.Fatalf("verdict = %+v ok=%v, want the raw copy refused", verdict, ok)
		}
	})

}

func TestInspectSnapshotTarEdgeCases(t *testing.T) {
	t.Run("a digest mismatch is judged from the stream", func(t *testing.T) {
		root := t.TempDir()
		writeTable(t, root, "probavi", "orders", tableFixture{corruptData: true})
		path := treeToTar(t, root, filepath.Join(t.TempDir(), "snap.tar"), "", false)
		_, verdict, ok := inspectSnapshotTar(path)
		if !ok || verdict == nil || verdict.Code != "source_corrupt" ||
			!strings.Contains(verdict.Message, "Digest.crc32") {
			t.Fatalf("verdict = %+v ok=%v, want the digest mismatch refused", verdict, ok)
		}
	})

	t.Run("an archive the reader cannot walk is a silent bonus miss", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "opaque.bin")
		if err := os.WriteFile(path, bytes.Repeat([]byte{0xA5}, 4096), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, ok := inspectSnapshotTar(path); ok {
			t.Error("random bytes walked as a tar")
		}
	})
}

func TestPlausibleEpochMs(t *testing.T) {
	for _, bad := range []int64{0, -1, 123, 946684799999, 7258118400001} {
		if plausibleEpochMs(bad) {
			t.Errorf("plausibleEpochMs(%d) = true", bad)
		}
	}
	if !plausibleEpochMs(fixtureCreatedMs) {
		t.Errorf("plausibleEpochMs(%d) = false", fixtureCreatedMs)
	}
}

func TestParseTOC(t *testing.T) {
	components := parseTOC([]byte("Data.db\n\nTOC.txt\n  Index.db  \n"))
	want := []string{"Data.db", "TOC.txt", "Index.db"}
	if len(components) != len(want) {
		t.Fatalf("parseTOC = %v, want %v", components, want)
	}
	for i := range want {
		if components[i] != want[i] {
			t.Fatalf("parseTOC = %v, want %v", components, want)
		}
	}
}

// TestInspectSnapshotTarRefusesUnboundedTableMetadata pins the retention
// bound. Every TOC, manifest and digest an archive carries is read at up
// to metaMaxBytes and kept per table, so without a bound a small archive
// decides how much memory the drill host spends. A backup file is
// attacker-controlled input (SECURITY.md), and a drill killed for memory
// leaves no evidence record — the severest failure this project defines.
// The directory kind needs no such bound: its bookkeeping is
// proportional to files that already exist on the operator's disk.
func TestInspectSnapshotTarRefusesUnboundedTableMetadata(t *testing.T) {
	body := strings.Repeat("a", metaMaxBytes)
	dest := filepath.Join(t.TempDir(), "snap.tar.gz")
	f, err := os.Create(dest)
	if err != nil {
		t.Fatal(err)
	}
	gzw := gzip.NewWriter(f)
	tw := tar.NewWriter(gzw)
	for i := 0; i <= keptMaxBytes/metaMaxBytes; i++ {
		name := fmt.Sprintf("probavi/orders/nb-%d-big%s", i, tocSuffix)
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []io.Closer{tw, gzw, f} {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}

	census, verdict, ok := inspectSnapshotTar(dest)
	if verdict == nil || !ok {
		t.Fatalf("inspectSnapshotTar = %+v, verdict %v, ok %v; want a refusal", census, verdict, ok)
	}
	if verdict.Code != "source_corrupt" {
		t.Errorf("code = %s, want source_corrupt", verdict.Code)
	}
	if !strings.Contains(verdict.Message, "memory") {
		t.Errorf("message %q must say why the walk stopped", verdict.Message)
	}
}
