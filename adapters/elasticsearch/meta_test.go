package main

import (
	"archive/zip"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fixtureIndex is an index-<gen> in the shape 8.19 and 9.5 write
// (measured): the writing engine named by index_version, the `version`
// beside it the snapshot format's own ("8.11.0" on both lines).
const fixtureIndex = `{"min_version":"7.12.0","snapshots":[` +
	`{"name":"snap-1","uuid":"u1","state":1,"version":"8.11.0","index_version":8537000,` +
	`"start_time_millis":1700000000000,"end_time_millis":1700000000000},` +
	`{"name":"snap-2","uuid":"u2","state":1,"version":"8.11.0","index_version":8537000,` +
	`"start_time_millis":1700000100000,"end_time_millis":1700000100000}],` +
	`"indices":{"orders":{"id":"i1","snapshots":["u1","u2"]}}}`

func writeRepoWithIndex(t *testing.T, dir, index string) string {
	t.Helper()
	blobDir := filepath.Join(dir, "indices", "i1", "0")
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	latest := make([]byte, 8)
	binary.BigEndian.PutUint64(latest, 0)
	writeFixtureFile(t, filepath.Join(dir, indexLatestName), latest)
	writeFixtureFile(t, filepath.Join(dir, "index-0"), []byte(index))
	writeFixtureFile(t, filepath.Join(blobDir, "__blob"), []byte("blob-bytes"))
	return dir
}

func writeRepo(t *testing.T, dir string) string {
	return writeRepoWithIndex(t, dir, fixtureIndex)
}

func writeFixtureFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
}

// treeToZip archives a directory the way `zip -r` does — entries under
// wrap when it is given (the wrapping-directory layout), at the root
// otherwise.
func treeToZip(t *testing.T, dir, dest, wrap string) string {
	t.Helper()
	f, err := os.Create(dest)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	err = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || rel == "." {
			return err
		}
		name := filepath.ToSlash(rel)
		if wrap != "" {
			name = wrap + "/" + name
		}
		if d.IsDir() {
			_, err := zw.Create(name + "/")
			return err
		}
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer src.Close() //nolint:errcheck // read side
		_, err = io.Copy(w, src)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return dest
}

func TestInspectRepoDirReadsTheCensus(t *testing.T) {
	dir := writeRepo(t, t.TempDir())
	census, perr := inspectRepoDir(dir)
	if perr != nil {
		t.Fatalf("inspectRepoDir: %+v", perr)
	}
	if got := census.names(); len(got) != 2 || got[0] != "snap-1" || got[1] != "snap-2" {
		t.Errorf("snapshot names = %v", got)
	}
	if len(census.indices) != 1 || census.indices[0] != "orders" {
		t.Errorf("indices = %v", census.indices)
	}
	if census.snapshots[0].IndexVersion != 8537000 {
		t.Errorf("index_version = %d, want the writing engine's own claim", census.snapshots[0].IndexVersion)
	}
}

func TestInspectRepoDirRefusals(t *testing.T) {
	base := t.TempDir()
	tests := []struct {
		name     string
		dir      func(t *testing.T) string
		wantCode string
		wantMsg  string
	}{
		{"a nodes directory is a live data directory", func(t *testing.T) string {
			d := filepath.Join(base, "live")
			if err := os.MkdirAll(filepath.Join(d, "nodes"), 0o755); err != nil {
				t.Fatal(err)
			}
			return d
		}, "unsupported_source", "nodes"},
		{"a _state directory is a live data directory", func(t *testing.T) string {
			d := filepath.Join(base, "state")
			if err := os.MkdirAll(filepath.Join(d, "_state"), 0o755); err != nil {
				t.Fatal(err)
			}
			return d
		}, "unsupported_source", "_state"},
		{"no index.latest is no repository", func(t *testing.T) string {
			d := filepath.Join(base, "plain")
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
			writeFixtureFile(t, filepath.Join(d, "data.bin"), []byte("x"))
			return d
		}, "source_corrupt", "index.latest"},
		{"a truncated index.latest", func(t *testing.T) string {
			d := writeRepo(t, filepath.Join(base, "truncated"))
			writeFixtureFile(t, filepath.Join(d, indexLatestName), []byte{0, 0, 0})
			return d
		}, "source_corrupt", "eight"},
		{"a generation the copy does not carry", func(t *testing.T) string {
			d := writeRepo(t, filepath.Join(base, "genless"))
			latest := make([]byte, 8)
			binary.BigEndian.PutUint64(latest, 7)
			writeFixtureFile(t, filepath.Join(d, indexLatestName), latest)
			return d
		}, "source_corrupt", "generation 7"},
		{"a generation file that is not the format's JSON", func(t *testing.T) string {
			return writeRepoWithIndex(t, filepath.Join(base, "badjson"), "not json")
		}, "source_corrupt", "does not parse"},
		{"a repository listing no snapshots", func(t *testing.T) string {
			return writeRepoWithIndex(t, filepath.Join(base, "empty"), `{"snapshots":[],"indices":{}}`)
		}, "source_corrupt", "no snapshots"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, perr := inspectRepoDir(tc.dir(t))
			if perr == nil || perr.Code != tc.wantCode || !strings.Contains(perr.Message, tc.wantMsg) {
				t.Errorf("verdict = %+v, want %s naming %q", perr, tc.wantCode, tc.wantMsg)
			}
		})
	}
}

func TestInspectRepoZipReadsBothLayouts(t *testing.T) {
	repo := writeRepo(t, t.TempDir())
	tests := []struct {
		name string
		path string
	}{
		{"repository at the root", treeToZip(t, repo, filepath.Join(t.TempDir(), "root.zip"), "")},
		{"repository under a wrapping directory", treeToZip(t, repo, filepath.Join(t.TempDir(), "wrapped.zip"), "repo")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			census, verdict, ok := inspectRepoZip(tc.path)
			if !ok || verdict != nil {
				t.Fatalf("ok=%v verdict=%+v", ok, verdict)
			}
			if got := census.names(); len(got) != 2 || got[1] != "snap-2" {
				t.Errorf("snapshot names = %v", got)
			}
			if census.snapshots[1].IndexVersion != 8537000 {
				t.Errorf("index_version = %d, want the archive's own claim", census.snapshots[1].IndexVersion)
			}
		})
	}
}

func TestInspectRepoZipVerdicts(t *testing.T) {
	t.Run("opaque bytes yield no claims and no verdict", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "opaque.bin")
		writeFixtureFile(t, path, []byte(strings.Repeat("x", 4096)))
		if _, verdict, ok := inspectRepoZip(path); ok || verdict != nil {
			t.Errorf("ok=%v verdict=%+v, want silence — the sandbox is the authority", ok, verdict)
		}
	})
	t.Run("a zip without index.latest stays with the sandbox", func(t *testing.T) {
		dir := t.TempDir()
		writeFixtureFile(t, filepath.Join(dir, "data.bin"), []byte("x"))
		path := treeToZip(t, dir, filepath.Join(t.TempDir(), "plain.zip"), "")
		if _, verdict, ok := inspectRepoZip(path); ok || verdict != nil {
			t.Errorf("ok=%v verdict=%+v, want silence", ok, verdict)
		}
	})
	t.Run("a nodes directory in the archive is a raw copy", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "nodes", "0"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFixtureFile(t, filepath.Join(dir, "nodes", "0", "node.lock"), []byte(""))
		path := treeToZip(t, dir, filepath.Join(t.TempDir(), "live.zip"), "data")
		_, verdict, ok := inspectRepoZip(path)
		if !ok || verdict == nil || verdict.Code != "unsupported_source" {
			t.Errorf("ok=%v verdict=%+v, want unsupported_source", ok, verdict)
		}
	})
}

func TestInspectRepoZipGenerationVerdicts(t *testing.T) {
	t.Run("an archive missing its named generation", func(t *testing.T) {
		repo := writeRepo(t, t.TempDir())
		if err := os.Remove(filepath.Join(repo, "index-0")); err != nil {
			t.Fatal(err)
		}
		path := treeToZip(t, repo, filepath.Join(t.TempDir(), "genless.zip"), "")
		_, verdict, ok := inspectRepoZip(path)
		if !ok || verdict == nil || verdict.Code != "source_corrupt" ||
			!strings.Contains(verdict.Message, "generation 0") {
			t.Errorf("ok=%v verdict=%+v, want source_corrupt naming the generation", ok, verdict)
		}
	})
	t.Run("an archived generation file that does not parse", func(t *testing.T) {
		repo := writeRepoWithIndex(t, t.TempDir(), "not json")
		path := treeToZip(t, repo, filepath.Join(t.TempDir(), "badjson.zip"), "")
		_, verdict, ok := inspectRepoZip(path)
		if !ok || verdict == nil || verdict.Code != "source_corrupt" {
			t.Errorf("ok=%v verdict=%+v, want source_corrupt", ok, verdict)
		}
	})
}

func TestIndexVersionNewer(t *testing.T) {
	tests := []struct {
		snapshot, engine int64
		want             bool
	}{
		{9111000, 8537000, true},
		{8537000, 8537000, false},
		{8537000, 9111000, false},
		{0, 8537000, false},
		{9111000, 0, false},
		{0, 0, false},
	}
	for _, tc := range tests {
		if got := indexVersionNewer(tc.snapshot, tc.engine); got != tc.want {
			t.Errorf("indexVersionNewer(%d, %d) = %v, want %v — the pre-check refuses on positive evidence only",
				tc.snapshot, tc.engine, got, tc.want)
		}
	}
}

// zipOfEntries writes a zip of synthetic entries: the shape a crafted
// archive takes, where each member costs the walk bookkeeping the
// archive itself barely pays for.
func zipOfEntries(t *testing.T, dest string, names []string, content []byte) string {
	t.Helper()
	f, err := os.Create(dest)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, name := range names {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []io.Closer{zw, f} {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return dest
}

// TestInspectRepoZipRefusesUnboundedGenerations pins the retention
// bound. This walk keeps directory entries rather than contents, so its
// own cost per member is small — but it is unbounded without this, and a
// backup file is attacker-controlled input. The sibling opensearch
// adapter, whose tar walk must retain the bytes, carries the same bound
// with a byte budget attached.
func TestInspectRepoZipRefusesUnboundedGenerations(t *testing.T) {
	names := make([]string, 0, keptMaxEntries+1)
	for i := 0; i <= keptMaxEntries; i++ {
		names = append(names, indexGenPrefix+strconv.Itoa(i))
	}
	path := zipOfEntries(t, filepath.Join(t.TempDir(), "repo.zip"), names, []byte("{}"))

	census, verdict, ok := inspectRepoZip(path)
	if verdict == nil || !ok {
		t.Fatalf("inspectRepoZip = %+v, verdict %v, ok %v; want a refusal", census, verdict, ok)
	}
	if verdict.Code != "source_corrupt" {
		t.Errorf("code = %s, want source_corrupt", verdict.Code)
	}
	if !strings.Contains(verdict.Message, indexGenPrefix) {
		t.Errorf("message %q must name what there is too much of", verdict.Message)
	}
}
