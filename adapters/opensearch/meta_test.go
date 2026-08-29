package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureIndex is a minimal index-0 in the measured shape: two snapshots
// written by 2.19.6 and one index they both contain.
const fixtureIndex = `{"snapshots":[` +
	`{"name":"snap-1","uuid":"u1","state":1,"version":"2.19.6"},` +
	`{"name":"snap-2","uuid":"u2","state":1,"version":"2.19.6"}],` +
	`"indices":{"orders":{"id":"i1","snapshots":["u1","u2"]}}}`

// writeRepoWithIndex writes a minimal fs snapshot repository: the
// generation pointer, the given index-0, and one blob under the layout
// the format uses.
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

// treeToTar tars a directory, optionally under one wrapping directory
// and optionally gzipped — the two layouts the adapter accepts.
func treeToTar(t *testing.T, dir, dest, wrap string, gz bool) string {
	t.Helper()
	f, err := os.Create(dest)
	if err != nil {
		t.Fatal(err)
	}
	var w io.Writer = f
	var gzw *gzip.Writer
	if gz {
		gzw = gzip.NewWriter(f)
		w = gzw
	}
	tw := tar.NewWriter(w)
	if err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		name := filepath.ToSlash(rel)
		if wrap != "" {
			name = wrap + "/" + name
		}
		return writeTarEntry(tw, p, name, d)
	}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if gzw != nil {
		if err := gzw.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return dest
}

func writeTarEntry(tw *tar.Writer, path, name string, d os.DirEntry) error {
	if d.IsDir() {
		return tw.WriteHeader(&tar.Header{Name: name + "/", Typeflag: tar.TypeDir, Mode: 0o755})
	}
	info, err := d.Info()
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: info.Size()}); err != nil {
		return err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	_, err = tw.Write(b)
	return err
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
	if census.snapshots[0].Version != "2.19.6" {
		t.Errorf("version = %q, want the writing engine's own claim", census.snapshots[0].Version)
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

func TestInspectRepoTarReadsBothLayouts(t *testing.T) {
	repo := writeRepo(t, t.TempDir())
	tests := []struct {
		name string
		path string
	}{
		{"plain tar at the root", treeToTar(t, repo, filepath.Join(t.TempDir(), "root.tar"), "", false)},
		{"gzip tar under a wrapping directory", treeToTar(t, repo, filepath.Join(t.TempDir(), "wrapped.tar.gz"), "backup", true)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			census, verdict, ok := inspectRepoTar(tc.path)
			if !ok || verdict != nil {
				t.Fatalf("ok=%v verdict=%+v", ok, verdict)
			}
			if got := census.names(); len(got) != 2 || got[1] != "snap-2" {
				t.Errorf("snapshot names = %v", got)
			}
		})
	}
}

func TestInspectRepoTarVerdicts(t *testing.T) {
	t.Run("an opaque archive yields no claims and no verdict", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "opaque.bin")
		writeFixtureFile(t, path, []byte(strings.Repeat("x", 4096)))
		if _, verdict, ok := inspectRepoTar(path); ok || verdict != nil {
			t.Errorf("ok=%v verdict=%+v, want silence — the sandbox is the authority", ok, verdict)
		}
	})
	t.Run("a tar without index.latest stays with the sandbox", func(t *testing.T) {
		dir := t.TempDir()
		writeFixtureFile(t, filepath.Join(dir, "data.bin"), []byte("x"))
		path := treeToTar(t, dir, filepath.Join(t.TempDir(), "plain.tar"), "", false)
		if _, verdict, ok := inspectRepoTar(path); ok || verdict != nil {
			t.Errorf("ok=%v verdict=%+v, want silence", ok, verdict)
		}
	})
	t.Run("a nodes directory in the archive is a raw copy", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "nodes", "0"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFixtureFile(t, filepath.Join(dir, "nodes", "0", "node.lock"), []byte(""))
		path := treeToTar(t, dir, filepath.Join(t.TempDir(), "live.tar"), "", false)
		_, verdict, ok := inspectRepoTar(path)
		if !ok || verdict == nil || verdict.Code != "unsupported_source" {
			t.Errorf("ok=%v verdict=%+v, want unsupported_source", ok, verdict)
		}
	})
}

func TestInspectRepoTarGenerationVerdicts(t *testing.T) {
	t.Run("an archive missing its named generation", func(t *testing.T) {
		repo := writeRepo(t, t.TempDir())
		if err := os.Remove(filepath.Join(repo, "index-0")); err != nil {
			t.Fatal(err)
		}
		path := treeToTar(t, repo, filepath.Join(t.TempDir(), "genless.tar"), "", false)
		_, verdict, ok := inspectRepoTar(path)
		if !ok || verdict == nil || verdict.Code != "source_corrupt" ||
			!strings.Contains(verdict.Message, "generation 0") {
			t.Errorf("ok=%v verdict=%+v, want source_corrupt naming the generation", ok, verdict)
		}
	})
	t.Run("an archived generation file that does not parse", func(t *testing.T) {
		repo := writeRepoWithIndex(t, t.TempDir(), "not json")
		path := treeToTar(t, repo, filepath.Join(t.TempDir(), "badjson.tar"), "", false)
		_, verdict, ok := inspectRepoTar(path)
		if !ok || verdict == nil || verdict.Code != "source_corrupt" {
			t.Errorf("ok=%v verdict=%+v, want source_corrupt", ok, verdict)
		}
	})
}

func TestVersionNewer(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"3.8.0", "2.19.6", true},
		{"2.19.6", "2.19.6", false},
		{"2.9.0", "2.19.6", false},
		{"2.19.7", "2.19.6", true},
		{"banana", "2.19.6", false},
		{"2.19.6", "", false},
		{"3", "2.19.6", false},
		{"", "", false},
	}
	for _, tc := range tests {
		if got := versionNewer(tc.a, tc.b); got != tc.want {
			t.Errorf("versionNewer(%q, %q) = %v, want %v — the pre-check refuses on positive evidence only",
				tc.a, tc.b, got, tc.want)
		}
	}
}

// tarOfEntries writes a gzipped tar of synthetic entries: the shape a
// crafted archive takes, where a 512-byte header compresses to nothing
// and the walk's own bookkeeping is the cost.
func tarOfEntries(t *testing.T, dest string, names []string, content []byte) string {
	t.Helper()
	f, err := os.Create(dest)
	if err != nil {
		t.Fatal(err)
	}
	gzw := gzip.NewWriter(f)
	tw := tar.NewWriter(gzw)
	for _, name := range names {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []io.Closer{tw, gzw, f} {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return dest
}

// TestInspectRepoTarRefusesUnboundedGenerations pins the retention
// bound. Every index-<gen> member is read and kept until index.latest
// says which one matters, so without a bound a small archive of many
// members decides how much memory the drill host spends — measured at
// 1.26 GiB resident from a 1.2 MB archive. The refusal is a verdict, not
// a silent skip: no repository root carries these.
func TestInspectRepoTarRefusesUnboundedGenerations(t *testing.T) {
	names := make([]string, 0, keptMaxEntries+1)
	for i := 0; i <= keptMaxEntries; i++ {
		names = append(names, fmt.Sprintf("%s%d", indexGenPrefix, i))
	}
	path := tarOfEntries(t, filepath.Join(t.TempDir(), "repo.tar.gz"), names, []byte("{}"))

	census, verdict, ok := inspectRepoTar(path)
	if verdict == nil || !ok {
		t.Fatalf("inspectRepoTar = %+v, verdict %v, ok %v; want a refusal", census, verdict, ok)
	}
	if verdict.Code != "source_corrupt" {
		t.Errorf("code = %s, want source_corrupt", verdict.Code)
	}
	if !strings.Contains(verdict.Message, indexGenPrefix) {
		t.Errorf("message %q must name what there is too much of", verdict.Message)
	}
}
