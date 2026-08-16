package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// meta.go reads what a snapshot states about itself, out of its own
// files — and judges each table against those statements. A
// `nodetool snapshot` writes, per table: the SSTable component files, a
// TOC.txt naming every component a generation consists of, a
// Digest.crc32 carrying the CRC-32 of the Data file, a schema.cql with
// the table's own DDL, and a manifest.json that lists the Data files and
// states when the snapshot was taken (measured on 4.1 and 5.0). Those
// self-claims matter because the restore tool is too forgiving to be
// trusted alone: sstableloader streams nothing and exits 0 when a
// component is missing, and streams a corrupted Data file without a word
// (both measured) — so completeness and integrity are judged here, from
// the artifact's own claims, before a byte is transferred.

// tableRef names one table of the artifact.
type tableRef struct {
	keyspace, table string
}

func (r tableRef) String() string { return r.keyspace + "." + r.table }

// snapshotCensus is what the artifact states about itself.
type snapshotCensus struct {
	// tables is every keyspace.table the artifact holds, sorted.
	tables []tableRef
	// maxCreatedMs is the newest manifest-stated snapshot instant, epoch
	// milliseconds; 0 when nothing plausible was read.
	maxCreatedMs int64
}

// tableManifest is the slice of a snapshot manifest.json this adapter
// reads (measured fields; 4.1 writes compact JSON, 5.0 indents).
type tableManifest struct {
	Files     []string `json:"files"`
	CreatedAt string   `json:"created_at"`
}

// tableFacts accumulates what one table directory states about itself;
// judgeTable turns it into a verdict. Both the filesystem walk and the
// archive stream fill the same shape.
type tableFacts struct {
	hasSchema  bool
	manifestOK bool
	manifest   tableManifest
	createdMs  int64
	entries    map[string]bool     // regular file names present
	tocs       map[string][]string // sstable prefix -> TOC component list
	digests    map[string]uint32   // sstable prefix -> Digest.crc32 claim
	dataCRC    map[string]uint32   // sstable prefix -> measured Data.db CRC
	liveMarker string              // "snapshots"/"backups" subdirectory seen
}

func newTableFacts() *tableFacts {
	return &tableFacts{
		entries: map[string]bool{}, tocs: map[string][]string{},
		digests: map[string]uint32{}, dataCRC: map[string]uint32{},
	}
}

// liveMarkers are the subdirectories only a live data directory's table
// tree contains — a snapshot's table directory is flat (measured).
var liveMarkers = map[string]bool{"snapshots": true, "backups": true}

// identifierShape is the unquoted CQL identifier this adapter accepts as
// a keyspace or table name. Directory names flow into composed CQL and
// argv, so anything else is refused rather than quoted around.
var identifierShape = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)

// systemKeyspaces are Cassandra's own; restoring them is never what a
// backup drill means, and a whole-data-directory rsync drags them in.
var systemKeyspaces = map[string]bool{
	"system": true, "system_schema": true, "system_auth": true,
	"system_distributed": true, "system_traces": true, "system_views": true,
	"system_virtual_schema": true,
}

const (
	manifestName = "manifest.json"
	schemaName   = "schema.cql"
	tocSuffix    = "-TOC.txt"
	digestSuffix = "-Digest.crc32"
	dataSuffix   = "-Data.db"
	// metaMaxBytes bounds manifest/TOC/digest reads; real ones are tiny.
	metaMaxBytes = 1 << 20
)

// judgeTable weighs one table against its own claims. Every refusal is
// positive evidence; a table that makes no claims (no TOC, no digest)
// passes here and the engine speaks later.
func judgeTable(ref tableRef, f *tableFacts) *protoError {
	switch {
	case f.liveMarker != "":
		return protoErr("unsupported_source", false,
			"%s contains a %q subdirectory, which only a live data directory holds: this is a raw "+
				"copy taken from under a running node, not a snapshot — take backups with "+
				"`nodetool snapshot` and collect each table's snapshots/<tag>/ directory instead",
			ref, f.liveMarker)
	case !f.hasSchema:
		return protoErr("source_corrupt", false,
			"%s carries no schema.cql: `nodetool snapshot` writes one per table, and without it "+
				"the table cannot be recreated from the backup's own claim — collect the whole "+
				"snapshots/<tag>/ directory of every table", ref)
	}
	if perr := judgeComponents(ref, f); perr != nil {
		return perr
	}
	return judgeDigests(ref, f)
}

// judgeComponents is the completeness census: every component a TOC or
// the manifest names must exist. This cannot be left to the restore —
// sstableloader finding no complete sstable streams nothing and exits 0
// (measured), the false pass a backup drill exists to catch.
func judgeComponents(ref tableRef, f *tableFacts) *protoError {
	for prefix, components := range f.tocs {
		for _, component := range components {
			if component == "TOC.txt" {
				continue
			}
			if !f.entries[prefix+"-"+component] {
				return protoErr("source_corrupt", false,
					"%s is missing %s-%s, which the sstable's own TOC.txt lists: an incomplete "+
						"sstable loads as nothing at all while the loader exits 0 (measured) — "+
						"the backup copy lost a file", ref, prefix, component)
			}
		}
	}
	if !f.manifestOK {
		return nil
	}
	for _, name := range f.manifest.Files {
		if !f.entries[name] {
			return protoErr("source_corrupt", false,
				"%s is missing %s, which the snapshot's own manifest.json lists — the backup "+
					"copy lost a file", ref, name)
		}
	}
	return nil
}

// judgeDigests verifies each Data file against the CRC-32 its own
// Digest.crc32 sidecar claims — the loader streams a corrupted Data file
// without a word, and the damage only surfaces when the restored table
// is read (both measured).
func judgeDigests(ref tableRef, f *tableFacts) *protoError {
	for prefix, claimed := range f.digests {
		measured, ok := f.dataCRC[prefix]
		if !ok {
			continue
		}
		if measured != claimed {
			return protoErr("source_corrupt", false,
				"%s: %s%s does not match the CRC-32 its own Digest.crc32 claims "+
					"(claimed %d, measured %d) — the backup's bytes changed since the snapshot "+
					"was taken, and the loader would stream the damage without a word (measured)",
				ref, prefix, dataSuffix, claimed, measured)
		}
	}
	return nil
}

// judgeName gates a keyspace or table directory name before it can reach
// composed CQL.
func judgeName(kind, name string) *protoError {
	if systemKeyspaces[name] {
		return protoErr("invalid_request", false,
			"the artifact contains the %s keyspace, which belongs to Cassandra itself: collect "+
				"only your application keyspaces — a drill restoring system tables proves nothing "+
				"about your data", name)
	}
	if !identifierShape.MatchString(name) {
		return protoErr("invalid_request", false,
			"%s name %q is not an unquoted CQL identifier this adapter accepts "+
				"([a-z][a-z0-9_]*, at most 48 characters): quoted or case-sensitive names are "+
				"not supported", kind, name)
	}
	return nil
}

// recordEntry files one regular file's name (and, when it is one of the
// self-describing pieces, its content) into the table's facts.
func recordEntry(f *tableFacts, name string, content func() ([]byte, error), dataCRC func() (uint32, error)) error {
	f.entries[name] = true
	switch {
	case name == schemaName:
		f.hasSchema = true
	case name == manifestName:
		raw, err := content()
		if err != nil {
			return err
		}
		recordManifest(f, raw)
	case strings.HasSuffix(name, tocSuffix):
		raw, err := content()
		if err != nil {
			return err
		}
		f.tocs[strings.TrimSuffix(name, tocSuffix)] = parseTOC(raw)
	case strings.HasSuffix(name, digestSuffix):
		raw, err := content()
		if err != nil {
			return err
		}
		if v, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 32); err == nil {
			f.digests[strings.TrimSuffix(name, digestSuffix)] = uint32(v)
		}
	case strings.HasSuffix(name, dataSuffix):
		crc, err := dataCRC()
		if err != nil {
			return err
		}
		f.dataCRC[strings.TrimSuffix(name, dataSuffix)] = crc
	}
	return nil
}

func recordManifest(f *tableFacts, raw []byte) {
	m := tableManifest{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return
	}
	f.manifestOK = true
	f.manifest = m
	if t, err := time.Parse(time.RFC3339Nano, m.CreatedAt); err == nil && plausibleEpochMs(t.UnixMilli()) {
		f.createdMs = t.UnixMilli()
	}
}

func parseTOC(raw []byte) []string {
	var components []string
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			components = append(components, line)
		}
	}
	return components
}

// plausibleEpochMs rejects values no snapshot instant produces, so a
// field that happens to parse cannot date a backup absurdly.
func plausibleEpochMs(ms int64) bool {
	const (
		year2000 = 946684800000
		year2200 = 7258118400000
	)
	return ms >= year2000 && ms <= year2200
}

// inspectSnapshotTree walks a collected snapshot tree — first level
// keyspaces, second level tables — judging every table against its own
// claims and returning the census the restore must satisfy.
func inspectSnapshotTree(root string) (snapshotCensus, *protoError) {
	keyspaces, err := os.ReadDir(root)
	if err != nil {
		return snapshotCensus{}, protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	census := snapshotCensus{}
	for _, ks := range keyspaces {
		if !ks.IsDir() {
			continue
		}
		if perr := judgeName("keyspace", ks.Name()); perr != nil {
			return snapshotCensus{}, perr
		}
		if perr := inspectKeyspaceDir(root, ks.Name(), &census); perr != nil {
			return snapshotCensus{}, perr
		}
	}
	if len(census.tables) == 0 {
		return snapshotCensus{}, protoErr("source_corrupt", false,
			"backup directory %s holds no keyspace/table snapshot directories — collect each "+
				"table's snapshots/<tag>/ directory as <keyspace>/<table>/ (the adapter README "+
				"shows the exact loop)", root)
	}
	sortTables(census.tables)
	return census, nil
}

func inspectKeyspaceDir(root, keyspace string, census *snapshotCensus) *protoError {
	tables, err := os.ReadDir(filepath.Join(root, keyspace))
	if err != nil {
		return protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	for _, table := range tables {
		if !table.IsDir() {
			continue
		}
		if perr := judgeName("table", table.Name()); perr != nil {
			return perr
		}
		ref := tableRef{keyspace: keyspace, table: table.Name()}
		facts, perr := tableFactsFromDir(filepath.Join(root, keyspace, table.Name()))
		if perr != nil {
			return perr
		}
		if perr := judgeTable(ref, facts); perr != nil {
			return perr
		}
		census.tables = append(census.tables, ref)
		if facts.createdMs > census.maxCreatedMs {
			census.maxCreatedMs = facts.createdMs
		}
	}
	return nil
}

func tableFactsFromDir(dir string) (*tableFacts, *protoError) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	facts := newTableFacts()
	for _, e := range entries {
		if e.IsDir() {
			if liveMarkers[e.Name()] {
				facts.liveMarker = e.Name()
			}
			continue
		}
		if !e.Type().IsRegular() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		err := recordEntry(facts, e.Name(),
			func() ([]byte, error) { return readCapped(path) },
			func() (uint32, error) { return fileCRC32(path) })
		if err != nil {
			return nil, protoErr("source_unreadable", false, "read %s: %v", e.Name(), err)
		}
	}
	return facts, nil
}

func readCapped(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	raw, rerr := io.ReadAll(io.LimitReader(f, metaMaxBytes))
	if cerr := f.Close(); rerr == nil {
		rerr = cerr
	}
	return raw, rerr
}

func fileCRC32(path string) (uint32, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	h := crc32.NewIEEE()
	_, rerr := io.Copy(h, f)
	if cerr := f.Close(); rerr == nil {
		rerr = cerr
	}
	return h.Sum32(), rerr
}

// inspectSnapshotTar reads what a snapshot archive states about itself in
// one streaming pass, without unpacking: the same census, judged the same
// way. Metadata is a bonus — an archive Go's tar reader cannot walk
// yields ok false and every verdict moves into the sandbox — except
// where an entry is positive evidence, which arrives as the verdict.
// Both the plain and the gzip form are read (measured: busybox-less
// debian tar in the image unpacks both too).
func inspectSnapshotTar(path string) (census snapshotCensus, verdict *protoError, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return snapshotCensus{}, nil, false
	}
	census, verdict, ok = walkTarFile(f)
	if err := f.Close(); err != nil {
		return snapshotCensus{}, nil, false
	}
	return census, verdict, ok
}

func walkTarFile(f *os.File) (snapshotCensus, *protoError, bool) {
	var r io.Reader = f
	head := make([]byte, 2)
	if _, err := io.ReadFull(f, head); err != nil {
		return snapshotCensus{}, nil, false
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return snapshotCensus{}, nil, false
	}
	if head[0] == 0x1f && head[1] == 0x8b {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return snapshotCensus{}, nil, false
		}
		r = gz
	}
	return walkTar(tar.NewReader(r))
}

// tarWalkState accumulates one archive walk.
type tarWalkState struct {
	tables    map[tableRef]*tableFacts
	sawEntry  bool
	depthBase int // segments before <keyspace>; learned from the first file
}

// walkTar accumulates per-table facts from the stream. A snapshot tars
// either the keyspace directories at its root or one wrapping directory
// above them; entries deeper than <wrap>/<ks>/<table>/<file> are not a
// collected snapshot's shape and end the bonus walk.
func walkTar(tr *tar.Reader) (snapshotCensus, *protoError, bool) {
	state := &tarWalkState{tables: map[tableRef]*tableFacts{}, depthBase: -1}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return snapshotCensus{}, nil, false
		}
		verdict, ok := walkTarEntry(state, hdr, tr)
		if verdict != nil {
			return snapshotCensus{}, verdict, true
		}
		if !ok {
			return snapshotCensus{}, nil, false
		}
	}
	if !state.sawEntry {
		return snapshotCensus{}, nil, false
	}
	return judgeTarTables(state.tables)
}

// walkTarEntry files one archive entry; a non-nil verdict is positive
// evidence, ok false ends the bonus walk silently.
func walkTarEntry(state *tarWalkState, hdr *tar.Header, tr *tar.Reader) (*protoError, bool) {
	segments := splitTarName(hdr.Name)
	if len(segments) == 0 {
		return nil, true
	}
	state.sawEntry = true
	if marker := tarLiveMarker(segments); marker != "" {
		return liveTarVerdict(marker), true
	}
	if hdr.Typeflag != tar.TypeReg {
		return nil, true
	}
	base, refSegments := segments[len(segments)-1], segments[:len(segments)-1]
	if state.depthBase == -1 {
		state.depthBase = len(refSegments) - 2
	}
	if len(refSegments)-2 != state.depthBase || state.depthBase < 0 || state.depthBase > 1 {
		return nil, false
	}
	ref := tableRef{keyspace: refSegments[state.depthBase], table: refSegments[state.depthBase+1]}
	facts, found := state.tables[ref]
	if !found {
		facts = newTableFacts()
		state.tables[ref] = facts
	}
	if err := recordTarEntry(facts, base, tr); err != nil {
		return nil, false
	}
	return nil, true
}

func recordTarEntry(facts *tableFacts, base string, tr *tar.Reader) error {
	return recordEntry(facts, base,
		func() ([]byte, error) { return io.ReadAll(io.LimitReader(tr, metaMaxBytes)) },
		func() (uint32, error) {
			h := crc32.NewIEEE()
			_, err := io.Copy(h, tr)
			return h.Sum32(), err
		})
}

func judgeTarTables(tables map[tableRef]*tableFacts) (snapshotCensus, *protoError, bool) {
	census := snapshotCensus{}
	for ref, facts := range tables {
		if perr := judgeName("keyspace", ref.keyspace); perr != nil {
			return snapshotCensus{}, perr, true
		}
		if perr := judgeName("table", ref.table); perr != nil {
			return snapshotCensus{}, perr, true
		}
		if perr := judgeTable(ref, facts); perr != nil {
			return snapshotCensus{}, perr, true
		}
		census.tables = append(census.tables, ref)
		if facts.createdMs > census.maxCreatedMs {
			census.maxCreatedMs = facts.createdMs
		}
	}
	if len(census.tables) == 0 {
		return snapshotCensus{}, protoErr("source_corrupt", false,
			"the archive holds no keyspace/table snapshot directories — not a collected "+
				"snapshot (the adapter README shows the exact collection loop)"), true
	}
	sortTables(census.tables)
	return census, nil, true
}

// tarLiveMarker reports a live-data-directory marker at the depths the
// two accepted layouts place a table's subdirectories.
func tarLiveMarker(segments []string) string {
	for depth := 2; depth < len(segments) && depth < 4; depth++ {
		if liveMarkers[segments[depth]] {
			return segments[depth]
		}
	}
	return ""
}

func liveTarVerdict(marker string) *protoError {
	return protoErr("unsupported_source", false,
		"the archive contains a %q subdirectory, which only a live data directory holds: this "+
			"is a tar of a raw data-directory copy, not of collected snapshots — take backups "+
			"with `nodetool snapshot` and collect each table's snapshots/<tag>/ directory instead",
		marker)
}

func splitTarName(name string) []string {
	name = strings.TrimPrefix(strings.TrimSuffix(name, "/"), "./")
	if name == "" {
		return nil
	}
	return strings.Split(name, "/")
}

func sortTables(tables []tableRef) {
	sort.Slice(tables, func(i, j int) bool {
		if tables[i].keyspace != tables[j].keyspace {
			return tables[i].keyspace < tables[j].keyspace
		}
		return tables[i].table < tables[j].table
	})
}
