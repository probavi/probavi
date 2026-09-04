package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// meta.go reads what an fs snapshot repository states about itself, out
// of its own files. The repository root carries `index.latest` — eight
// big-endian bytes naming the current generation — and `index-<gen>`, a
// JSON document listing every snapshot (name, uuid, state, and the
// OpenSearch version that wrote it) and every index with its snapshot
// membership (all measured on 2.19 and 3.8). Those self-claims are the
// host side of the fences: a directory that is not a repository is
// refused before a byte is transferred — the engine itself registers a
// garbage directory silently and lists zero snapshots (measured), which
// is exactly the false green the census closes — and the writing
// versions feed the pairing refusal before the engine's own one.

// repoSnapshot is one snapshot the repository's index-<gen> lists.
type repoSnapshot struct {
	Name    string `json:"name"`
	UUID    string `json:"uuid"`
	State   int    `json:"state"`
	Version string `json:"version"`
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
	// keptMaxBytes and keptMaxEntries bound what one archive walk holds
	// on to across entries. A tar entry is a 512-byte header that
	// compresses to almost nothing, so a small archive can carry
	// thousands of index-<gen> members and each is read up to
	// metaMaxBytes: measured at 1.26 GiB resident from a 1.2 MB archive
	// before these existed, and a backup file is attacker-controlled
	// input (SECURITY.md). A repository root names one current
	// generation, so nothing real comes close to either bound.
	keptMaxBytes   = 64 << 20
	keptMaxEntries = 4096
)

// retention accounts for what a walk keeps rather than for what it
// reads: an archive may hold any number of entries this pass ignores,
// and refusing those would turn a large legitimate copy into a failed
// drill.
type retention struct {
	entries int
	bytes   int
}

// take accounts for one retained entry of n bytes and reports whether
// the walk may keep it.
func (r *retention) take(n int) bool {
	r.entries++
	r.bytes += n
	return r.entries <= keptMaxEntries && r.bytes <= keptMaxBytes
}

// tooMuchKept is the refusal both archive walks share.
func tooMuchKept() *protoError {
	return protoErr("source_corrupt", false,
		"the archive carries more %s members than a snapshot repository holds — over %d of them, "+
			"or more than %d MiB in total. A repository root names one current generation, so this "+
			"is not a copy of one, and reading on would cost the drill host memory an archive gets "+
			"to choose",
		indexGenPrefix, keptMaxEntries, keptMaxBytes>>20)
}

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

// inspectRepoTar reads the same claims out of a tar of the repository,
// in one streaming pass, without unpacking. Metadata is a bonus — an
// archive Go's tar reader cannot walk yields ok false and every verdict
// moves into the sandbox — except where an entry is positive evidence,
// which arrives as the verdict. Both the plain and the gzip form are
// read.
func inspectRepoTar(path string) (census repoCensus, verdict *protoError, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return repoCensus{}, nil, false
	}
	census, verdict, ok = walkTarFile(f)
	if err := f.Close(); err != nil {
		return repoCensus{}, nil, false
	}
	return census, verdict, ok
}

func walkTarFile(f *os.File) (repoCensus, *protoError, bool) {
	var r io.Reader = f
	head := make([]byte, 2)
	if _, err := io.ReadFull(f, head); err != nil {
		return repoCensus{}, nil, false
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return repoCensus{}, nil, false
	}
	if head[0] == 0x1f && head[1] == 0x8b {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return repoCensus{}, nil, false
		}
		r = gz
	}
	return walkTar(tar.NewReader(r))
}

// tarScan accumulates the generation files a streaming pass surfaces.
type tarScan struct {
	sawEntry    bool
	generations map[string][]byte
	latest      []byte
	kept        retention
}

// walkTar scans the archive: the repository sits at the root or under
// one wrapping directory, so index.latest is expected at depth one or
// two, and generation files beside it.
func walkTar(tr *tar.Reader) (repoCensus, *protoError, bool) {
	scan := &tarScan{generations: map[string][]byte{}}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return repoCensus{}, nil, false
		}
		verdict, ok := scan.takeEntry(tr, hdr)
		if verdict != nil || !ok {
			return repoCensus{}, verdict, verdict != nil
		}
	}
	if !scan.sawEntry || len(scan.latest) != 8 {
		return repoCensus{}, nil, false
	}
	latest, generations := scan.latest, scan.generations
	gen := binary.BigEndian.Uint64(latest)
	raw, found := generations[fmt.Sprintf("%s%d", indexGenPrefix, gen)]
	if !found {
		return repoCensus{}, protoErr("source_corrupt", false,
			"the archive's index.latest names generation %d, but index-%d is not in the archive — "+
				"the repository copy is incomplete", gen, gen), true
	}
	census, perr := parseRepoIndex(raw)
	if perr != nil {
		return repoCensus{}, perr, true
	}
	return census, nil, true
}

// takeEntry inspects one archive entry: a live marker is the verdict, a
// root-level generation file is collected, an unreadable stream ends
// the pass silently (ok false).
func (s *tarScan) takeEntry(tr *tar.Reader, hdr *tar.Header) (*protoError, bool) {
	segments := splitTarName(hdr.Name)
	if len(segments) == 0 {
		return nil, true
	}
	s.sawEntry = true
	if marker := tarLiveMarker(segments); marker != "" {
		return protoErr("unsupported_source", false,
			"the archive contains a %q directory, which only a live data directory holds: this "+
				"is a tar of a raw data-directory copy, not of an fs snapshot repository — "+
				"register an fs repository, snapshot into it, and tar that directory instead",
			marker), true
	}
	if hdr.Typeflag != tar.TypeReg || len(segments) > 2 {
		return nil, true
	}
	base := segments[len(segments)-1]
	switch {
	case base == indexLatestName:
		raw, err := io.ReadAll(io.LimitReader(tr, 16))
		if err != nil {
			return nil, false
		}
		s.latest = raw
	case strings.HasPrefix(base, indexGenPrefix):
		raw, err := io.ReadAll(io.LimitReader(tr, metaMaxBytes))
		if err != nil {
			return nil, false
		}
		if !s.kept.take(len(base) + len(raw)) {
			return tooMuchKept(), true
		}
		s.generations[base] = raw
	}
	return nil, true
}

// tarLiveMarker reports a live-data-directory marker at the depths the
// two accepted layouts place top-level entries.
func tarLiveMarker(segments []string) string {
	for depth := 0; depth < len(segments)-1 && depth < 2; depth++ {
		if liveMarkers[segments[depth]] {
			return segments[depth]
		}
	}
	return ""
}

func splitTarName(name string) []string {
	name = strings.TrimPrefix(strings.TrimSuffix(name, "/"), "./")
	if name == "" {
		return nil
	}
	return strings.Split(name, "/")
}

// maxVersionComponent is the largest value a version component may reach
// before another digit is refused. No engine numbers a release this high,
// and the bound is what keeps the accumulator below the point where it
// stops meaning the digits it read.
const maxVersionComponent = 1 << 20

// versionTriple parses an OpenSearch version string's numeric triple.
func versionTriple(v string) (parts [3]int, ok bool) {
	fields := strings.SplitN(strings.TrimSpace(v), ".", 3)
	if len(fields) != 3 {
		return parts, false
	}
	for i, f := range fields {
		n := 0
		for _, r := range f {
			// A component is refused whole: a digit run long enough to
			// overflow the accumulator would compare as a small version,
			// which is the one direction that matters — this parse gates
			// the refusal of a snapshot written by a newer server.
			if r < '0' || r > '9' || n > maxVersionComponent {
				return [3]int{}, false
			}
			n = n*10 + int(r-'0')
		}
		parts[i] = n
	}
	return parts, true
}

// versionNewer reports a > b, when both parse; anything unparseable
// compares as not newer — the pre-check refuses on positive evidence
// only, and the engine's own refusal stays the authority.
func versionNewer(a, b string) bool {
	av, aok := versionTriple(a)
	bv, bok := versionTriple(b)
	if !aok || !bok {
		return false
	}
	for i := range av {
		if av[i] != bv[i] {
			return av[i] > bv[i]
		}
	}
	return false
}
