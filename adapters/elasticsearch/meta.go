package main

import (
	"archive/zip"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// meta.go reads what an fs snapshot repository states about itself, out
// of its own files. The repository root carries `index.latest` — eight
// big-endian bytes naming the current generation — and `index-<gen>`, a
// JSON document listing every snapshot and every index with its snapshot
// membership (measured on 8.19 and 9.5). Those self-claims are the host
// side of the fences: a directory that is not a repository is refused
// before a byte is transferred — the engine itself registers a garbage
// directory silently and lists zero snapshots (measured), which is
// exactly the false green the census closes — and the writing versions
// feed the pairing refusal before the engine's own one.
//
// Elasticsearch names the writing engine by its index version, an
// integer (`index_version`, 8537000 for 8.19.20 and 9111000 for 9.5.2,
// measured), not by a release string: the `version` field beside it is
// the snapshot format's own version and reads "8.11.0" on both lines.
// The sandbox node states its own index version through `_nodes`, so
// the pairing compares two integers and names both.

// repoSnapshot is one snapshot the repository's index-<gen> lists.
type repoSnapshot struct {
	Name         string `json:"name"`
	UUID         string `json:"uuid"`
	State        int    `json:"state"`
	IndexVersion int64  `json:"index_version"`
}

// repoIndexEntry is one index with the snapshot uuids that contain it.
type repoIndexEntry struct {
	ID        string   `json:"id"`
	Snapshots []string `json:"snapshots"`
}

// repoIndex is the slice of index-<gen> this adapter reads.
type repoIndex struct {
	Snapshots []repoSnapshot            `json:"snapshots"`
	Indices   map[string]repoIndexEntry `json:"indices"`
}

// repoCensus is what the artifact states about itself.
type repoCensus struct {
	// snapshots is every snapshot the repository lists, in file order.
	snapshots []repoSnapshot
	// indices is every index name the repository lists.
	indices []string
}

// names returns the snapshot names, for messages.
func (c repoCensus) names() []string {
	names := make([]string, 0, len(c.snapshots))
	for _, s := range c.snapshots {
		names = append(names, s.Name)
	}
	return names
}

const (
	indexLatestName = "index.latest"
	indexGenPrefix  = "index-"
	// metaMaxBytes bounds an index-<gen> read; real ones are small.
	metaMaxBytes = 8 << 20
	// keptMaxEntries bounds how many index-<gen> members one walk holds
	// on to. This walk keeps directory entries rather than contents —
	// the generation itself is read once, after index.latest names it —
	// so the archive's own central directory, which archive/zip parses
	// before this code runs, is the larger cost. The bound is here so
	// that a backup file, which SECURITY.md names as attacker-controlled
	// input, cannot buy unbounded bookkeeping on top of it; the sibling
	// opensearch adapter, whose tar walk must retain contents, carries
	// the same bound with bytes attached.
	keptMaxEntries = 4096
)

// liveMarkers are directory entries only an engine's live data directory
// contains — an fs repository holds generations, blobs and metadata,
// never these.
var liveMarkers = map[string]bool{"nodes": true, "_state": true}

// inspectRepoDir vets a repository directory by what its own files
// state.
func inspectRepoDir(dir string) (repoCensus, *protoError) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return repoCensus{}, protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() && liveMarkers[e.Name()] {
			return repoCensus{}, protoErr("unsupported_source", false,
				"%s contains a %q directory, which only a live data directory holds: this is a raw "+
					"copy taken from under a running node, not a snapshot repository — register an fs "+
					"repository, snapshot into it, and point the drill at that directory instead",
				dir, e.Name())
		}
	}
	gen, perr := readGeneration(filepath.Join(dir, indexLatestName), dir)
	if perr != nil {
		return repoCensus{}, perr
	}
	raw, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("%s%d", indexGenPrefix, gen)))
	if err != nil {
		return repoCensus{}, protoErr("source_corrupt", false,
			"the repository's index.latest names generation %d, but index-%d cannot be read (%v) — "+
				"the repository copy is incomplete", gen, gen, err)
	}
	return parseRepoIndex(raw)
}

// readGeneration reads index.latest: eight big-endian bytes (measured).
func readGeneration(path, dir string) (uint64, *protoError) {
	raw, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		return 0, protoErr("source_corrupt", false,
			"%s carries no index.latest — not an fs snapshot repository (the engine registers such "+
				"a directory silently and lists zero snapshots, measured, which is why this is "+
				"refused here by name)", dir)
	case err != nil:
		return 0, protoErr("source_unreadable", false, "read index.latest: %v", err)
	case len(raw) != 8:
		return 0, protoErr("source_corrupt", false,
			"index.latest is %d bytes, not the eight the format writes — the repository copy is "+
				"damaged", len(raw))
	}
	return binary.BigEndian.Uint64(raw), nil
}

func parseRepoIndex(raw []byte) (repoCensus, *protoError) {
	idx := repoIndex{}
	if err := json.Unmarshal(raw, &idx); err != nil {
		return repoCensus{}, protoErr("source_corrupt", false,
			"the repository's generation file does not parse as the format's JSON (%v) — the "+
				"repository copy is damaged", err)
	}
	if len(idx.Snapshots) == 0 {
		return repoCensus{}, protoErr("source_corrupt", false,
			"the repository lists no snapshots — nothing to restore, and nothing this drill could "+
				"honestly prove")
	}
	census := repoCensus{snapshots: idx.Snapshots}
	for name := range idx.Indices {
		census.indices = append(census.indices, name)
	}
	return census, nil
}

// inspectRepoZip reads the same claims out of a zip of the repository,
// through the archive's central directory, without unpacking. Metadata
// is a bonus — bytes Go's zip reader cannot open yield ok false and every
// verdict moves into the sandbox — except where an entry is positive
// evidence, which arrives as the verdict.
func inspectRepoZip(path string) (census repoCensus, verdict *protoError, ok bool) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return repoCensus{}, nil, false
	}
	census, verdict, ok = walkZip(&zr.Reader)
	if err := zr.Close(); err != nil {
		return repoCensus{}, nil, false
	}
	return census, verdict, ok
}

// zipScan accumulates the generation files the archive carries.
type zipScan struct {
	sawEntry    bool
	generations map[string]*zip.File
	latest      *zip.File
	kept        int
}

// walkZip scans the archive's directory: the repository sits at the root
// or under one wrapping directory, so index.latest is expected at depth
// one or two, and generation files beside it.
func walkZip(zr *zip.Reader) (repoCensus, *protoError, bool) {
	scan := &zipScan{generations: map[string]*zip.File{}}
	for _, f := range zr.File {
		if verdict := scan.takeEntry(f); verdict != nil {
			return repoCensus{}, verdict, true
		}
	}
	if !scan.sawEntry || scan.latest == nil {
		return repoCensus{}, nil, false
	}
	latest, err := readZipEntry(scan.latest, 16)
	if err != nil || len(latest) != 8 {
		return repoCensus{}, nil, false
	}
	gen := binary.BigEndian.Uint64(latest)
	entry, found := scan.generations[fmt.Sprintf("%s%d", indexGenPrefix, gen)]
	if !found {
		return repoCensus{}, protoErr("source_corrupt", false,
			"the archive's index.latest names generation %d, but index-%d is not in the archive — "+
				"the repository copy is incomplete", gen, gen), true
	}
	raw, err := readZipEntry(entry, metaMaxBytes)
	if err != nil {
		return repoCensus{}, nil, false
	}
	census, perr := parseRepoIndex(raw)
	if perr != nil {
		return repoCensus{}, perr, true
	}
	return census, nil, true
}

// takeEntry inspects one archive entry: a live marker is the verdict, a
// root-level generation file is collected.
func (s *zipScan) takeEntry(f *zip.File) *protoError {
	segments := splitArchiveName(f.Name)
	if len(segments) == 0 {
		return nil
	}
	s.sawEntry = true
	if marker := archiveLiveMarker(segments); marker != "" {
		return protoErr("unsupported_source", false,
			"the archive contains a %q directory, which only a live data directory holds: this "+
				"is a zip of a raw data-directory copy, not of an fs snapshot repository — "+
				"register an fs repository, snapshot into it, and zip that directory instead",
			marker)
	}
	if f.FileInfo().IsDir() || len(segments) > 2 {
		return nil
	}
	base := segments[len(segments)-1]
	switch {
	case base == indexLatestName:
		s.latest = f
	case strings.HasPrefix(base, indexGenPrefix):
		s.kept++
		if s.kept > keptMaxEntries {
			return protoErr("source_corrupt", false,
				"the archive carries more %s members than a snapshot repository holds — over %d of "+
					"them. A repository root names one current generation, so this is not a copy of "+
					"one, and reading on would cost the drill host memory an archive gets to choose",
				indexGenPrefix, keptMaxEntries)
		}
		s.generations[base] = f
	}
	return nil
}

func readZipEntry(f *zip.File, limit int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(rc, limit))
	if cerr := rc.Close(); cerr != nil && err == nil {
		err = cerr
	}
	return raw, err
}

// archiveLiveMarker reports a live-data-directory marker at the depths
// the two accepted layouts place top-level entries.
func archiveLiveMarker(segments []string) string {
	for depth := 0; depth < len(segments)-1 && depth < 2; depth++ {
		if liveMarkers[segments[depth]] {
			return segments[depth]
		}
	}
	return ""
}

func splitArchiveName(name string) []string {
	name = strings.TrimPrefix(strings.TrimSuffix(name, "/"), "./")
	if name == "" {
		return nil
	}
	return strings.Split(name, "/")
}

// indexVersionNewer reports that a snapshot's writing index version is
// newer than the engine's, when both are known; an unknown side (zero)
// compares as not newer — the pre-check refuses on positive evidence
// only, and the engine's own refusal stays the authority.
func indexVersionNewer(snapshot, engine int64) bool {
	return snapshot > 0 && engine > 0 && snapshot > engine
}
