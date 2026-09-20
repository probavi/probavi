package main

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// tableRef names one restorable table.
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

// scyllaManifest is the slice of a snapshot manifest.json this adapter
// reads. The engine writes considerably more than its Cassandra
// ancestor, and two of the extras carry the adapter:
//
//   - snapshot.created_at is epoch seconds, so the backup dates itself
//     exactly and no timezone has to be declared for it;
//   - sstables[] names every component set the snapshot should hold, so
//     completeness is a comparison rather than a guess.
//
// Fields this adapter does not read (node identity, token ranges,
// repaired_at) are left out deliberately: what is not used is not
// claimed.
type scyllaManifest struct {
	Snapshot struct {
		Name      string `json:"name"`
		CreatedAt int64  `json:"created_at"`
	} `json:"snapshot"`
	Table struct {
		KeyspaceName string `json:"keyspace_name"`
		TableName    string `json:"table_name"`
		TabletCount  int    `json:"tablet_count"`
	} `json:"table"`
	SSTables []struct {
		TOCName  string `json:"toc_name"`
		DataSize int64  `json:"data_size"`
		TabletID *int   `json:"tablet_id"`
	} `json:"sstables"`
}

// tableFacts accumulates what one table directory states about itself;
// judgeTable turns it into a verdict. Both the filesystem walk and the
// archive stream fill the same shape.
type tableFacts struct {
	hasSchema  bool
	manifestOK bool
	manifest   scyllaManifest
	createdMs  int64
	entries    map[string]bool // regular file names present
	liveMarker string          // "snapshots"/"backups" subdirectory seen
}

func newTableFacts() *tableFacts {
	return &tableFacts{entries: map[string]bool{}}
}

// liveMarkers are the subdirectories only a live data directory's table
// tree contains — a snapshot's table directory is flat.
var liveMarkers = map[string]bool{"snapshots": true, "backups": true, "upload": true, "staging": true}

// identifierShape is the unquoted CQL identifier this adapter accepts as
// a keyspace or table name. Directory names flow into composed CQL and
// argv, so anything else is refused rather than quoted around.
var identifierShape = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)

// systemKeyspaces are the engine's own; restoring them is never what a
// backup drill means, and a whole-data-directory copy drags them in.
var systemKeyspaces = map[string]bool{
	"system": true, "system_schema": true, "system_auth": true,
	"system_distributed": true, "system_distributed_everywhere": true,
	"system_traces": true, "system_replicated_keys": true, "audit": true,
}

const (
	manifestName = "manifest.json"
	schemaName   = "schema.cql"
	tocSuffix    = "-TOC.txt"
	// metaMaxBytes bounds manifest reads; real ones are tiny.
	metaMaxBytes = 1 << 20
	// keptMaxBytes and keptMaxEntries bound what one archive walk holds
	// on to across entries. A tar entry is a 512-byte header that
	// compresses to almost nothing, so a small archive can carry any
	// number of them, and a backup file is attacker-controlled input
	// (SECURITY.md). The bound is set against a real snapshot's shape:
	// this engine writes one sstable per tablet, so a large table runs
	// to thousands of component files, and nothing real approaches this.
	// The directory kind is not bounded here and does not need to be:
	// its bookkeeping is proportional to files already on the operator's
	// own disk, with no archive in between to multiply them.
	keptMaxBytes   = 64 << 20
	keptMaxEntries = 200_000
)

// retention accounts for what a walk keeps rather than for what it reads.
type retention struct {
	entries int
	bytes   int
}

func (r *retention) take(n int) bool {
	r.entries++
	r.bytes += n
	return r.entries <= keptMaxEntries && r.bytes <= keptMaxBytes
}

func tooMuchKept() *protoError {
	return protoErr("source_corrupt", false,
		"the archive's metadata exceeds what a collected snapshot carries")
}

// judgeTable turns one table's facts into a verdict. The order matters:
// a live data directory is named as such before anything else, because
// every later complaint would be a confusing way to say it.
func judgeTable(ref tableRef, f *tableFacts) *protoError {
	if f.liveMarker != "" {
		return protoErr("invalid_request", false,
			"%s looks like a live data directory (it holds a %s/ subdirectory), not a collected "+
				"snapshot: point the drill at a snapshot's own contents", ref, f.liveMarker)
	}
	if !f.hasSchema {
		return protoErr("source_corrupt", false,
			"%s holds no %s — a collected snapshot carries the table's own schema beside its "+
				"sstables, and without it the drill cannot recreate the table", ref, schemaName)
	}
	if !f.manifestOK {
		return protoErr("source_corrupt", false,
			"%s holds no readable %s — the engine writes one with every snapshot, and it is what "+
				"says whether the copy is complete", ref, manifestName)
	}
	return judgeComponents(ref, f)
}

// judgeComponents holds the copy against the manifest's own list. The
// engine writes one sstable per tablet and names each set's TOC in the
// manifest, so a copy that lost a tablet's files is detectable before a
// byte is restored — which is the failure a directory listing cannot see,
// because what remains still looks like a snapshot.
func judgeComponents(ref tableRef, f *tableFacts) *protoError {
	missing := []string{}
	for _, s := range f.manifest.SSTables {
		if s.TOCName == "" {
			continue
		}
		if !f.entries[s.TOCName] {
			missing = append(missing, s.TOCName)
			continue
		}
		prefix := strings.TrimSuffix(s.TOCName, tocSuffix)
		if !f.entries[prefix+"-Data.db"] {
			missing = append(missing, prefix+"-Data.db")
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		shown := missing
		if len(shown) > 3 {
			shown = shown[:3]
		}
		return protoErr("source_corrupt", false,
			"%s is missing %d of the %d files its own %s lists (%s): the copy is incomplete, and "+
				"restoring it would prove a backup nobody has", ref, len(missing),
			len(f.manifest.SSTables), manifestName, strings.Join(shown, ", "))
	}
	return nil
}

// judgeName refuses a keyspace or table name that is not an unquoted CQL
// identifier, or one of the engine's own keyspaces.
func judgeName(kind, name string) *protoError {
	if !identifierShape.MatchString(name) {
		return protoErr("invalid_request", false,
			"%s name %q is not an unquoted CQL identifier: this adapter restores snapshots whose "+
				"directory names it can put into CQL unchanged", kind, name)
	}
	if kind == "keyspace" && systemKeyspaces[name] {
		return protoErr("invalid_request", false,
			"%s is one of the engine's own keyspaces: a drill restores the operator's data, and "+
				"a copy of a whole data directory carries these along with it", name)
	}
	return nil
}

// recordManifest reads the snapshot manifest and keeps the instant it
// states. A manifest that does not parse leaves manifestOK false, which
// judgeTable reports in its own words.
func recordManifest(f *tableFacts, raw []byte) {
	m := scyllaManifest{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return
	}
	f.manifestOK = true
	f.manifest = m
	if ms := m.Snapshot.CreatedAt * 1000; plausibleEpochMs(ms) {
		f.createdMs = ms
	}
}

// plausibleEpochMs rejects an instant no snapshot of a running engine
// could carry, so a corrupt or hostile manifest cannot date a record.
func plausibleEpochMs(ms int64) bool {
	const y2015 = 1420070400000
	const y2100 = 4102444800000
	return ms > y2015 && ms < y2100
}

// inspectSnapshotTree reads a collected snapshot tree on the host.
func inspectSnapshotTree(root string) (snapshotCensus, *protoError) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return snapshotCensus{}, protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	census := snapshotCensus{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if perr := inspectKeyspaceDir(root, e.Name(), &census); perr != nil {
			return snapshotCensus{}, perr
		}
	}
	if len(census.tables) == 0 {
		return snapshotCensus{}, protoErr("source_corrupt", false,
			"%s holds no <keyspace>/<table>/ directories — not a collected snapshot", root)
	}
	sortTables(census.tables)
	return census, nil
}

func inspectKeyspaceDir(root, keyspace string, census *snapshotCensus) *protoError {
	if perr := judgeName("keyspace", keyspace); perr != nil {
		return perr
	}
	entries, err := os.ReadDir(filepath.Join(root, keyspace))
	if err != nil {
		return protoErr("source_unreadable", false, "read keyspace directory %s: %v", keyspace, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if perr := judgeName("table", e.Name()); perr != nil {
			return perr
		}
		ref := tableRef{keyspace: keyspace, table: e.Name()}
		facts, perr := tableFactsFromDir(filepath.Join(root, keyspace, e.Name()))
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
		return nil, protoErr("source_unreadable", false, "read table directory %s: %v", dir, err)
	}
	f := newTableFacts()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			if liveMarkers[name] && f.liveMarker == "" {
				f.liveMarker = name
			}
			continue
		}
		f.entries[name] = true
		switch name {
		case schemaName:
			f.hasSchema = true
		case manifestName:
			raw, err := readCapped(filepath.Join(dir, name))
			if err != nil {
				return nil, protoErr("source_unreadable", false, "read %s: %v", name, err)
			}
			recordManifest(f, raw)
		}
	}
	return f, nil
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

func sortTables(tables []tableRef) {
	sort.Slice(tables, func(i, j int) bool {
		if tables[i].keyspace != tables[j].keyspace {
			return tables[i].keyspace < tables[j].keyspace
		}
		return tables[i].table < tables[j].table
	})
}

func sortStrings(s []string) { sort.Strings(s) }

// treeFile is one file the host will put into the sandbox.
type treeFile struct{ host, dest string }

// walkTree lists the directories and files of a snapshot tree, so the
// transfer is one mkdir plus one put_file per file.
func walkTree(hostDir, stageRoot string) ([]string, []treeFile, *protoError) {
	dirs := []string{stageRoot}
	var files []treeFile
	err := filepath.WalkDir(hostDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(hostDir, p)
		if err != nil || rel == "." {
			return err
		}
		slash := filepath.ToSlash(rel)
		if d.IsDir() {
			dirs = append(dirs, stageRoot+"/"+slash)
		} else if d.Type().IsRegular() {
			files = append(files, treeFile{host: p, dest: stageRoot + "/" + slash})
		}
		return nil
	})
	if err != nil {
		return nil, nil, protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	return dirs, files, nil
}

// inspectSnapshotTar reads what the host can out of an archive in one
// streaming pass, and falls silent where the stream is not tar-shaped:
// the sandbox extraction is then the authority, because metadata is a
// bonus and verdicts are not guesses.
func inspectSnapshotTar(path string) (census snapshotCensus, verdict *protoError, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return snapshotCensus{}, nil, false
	}
	census, verdict, ok = walkTar(tar.NewReader(f))
	if cerr := f.Close(); cerr != nil && verdict == nil {
		// The walk is a bonus; a close that failed says nothing about
		// the artifact, so it costs the census rather than the drill.
		return snapshotCensus{}, nil, false
	}
	return census, verdict, ok
}

type tarWalkState struct {
	tables map[tableRef]*tableFacts
	kept   retention
}

func walkTar(tr *tar.Reader) (snapshotCensus, *protoError, bool) {
	state := &tarWalkState{tables: map[tableRef]*tableFacts{}}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// Not tar-shaped, or damaged past this point: say nothing and
			// let the sandbox's own tar be the authority.
			return snapshotCensus{}, nil, false
		}
		if perr, cont := walkTarEntry(state, hdr, tr); perr != nil {
			return snapshotCensus{}, perr, true
		} else if !cont {
			return snapshotCensus{}, nil, false
		}
	}
	return judgeTarTables(state.tables)
}

func walkTarEntry(state *tarWalkState, hdr *tar.Header, tr *tar.Reader) (*protoError, bool) {
	segments := splitTarName(hdr.Name)
	if len(segments) < 3 {
		return nil, true
	}
	// <keyspace>/<table>/<file>, optionally under one wrapping directory.
	if len(segments) > 3 {
		segments = segments[len(segments)-3:]
	}
	ref := tableRef{keyspace: segments[0], table: segments[1]}
	base := segments[2]
	if !identifierShape.MatchString(ref.keyspace) || !identifierShape.MatchString(ref.table) {
		return nil, true
	}
	facts := state.tables[ref]
	if facts == nil {
		facts = newTableFacts()
		state.tables[ref] = facts
	}
	if hdr.Typeflag == tar.TypeDir {
		if liveMarkers[base] && facts.liveMarker == "" {
			facts.liveMarker = base
		}
		return nil, true
	}
	if hdr.Typeflag != tar.TypeReg {
		return nil, true
	}
	if !state.kept.take(len(base)) {
		return tooMuchKept(), true
	}
	facts.entries[base] = true
	switch base {
	case schemaName:
		facts.hasSchema = true
	case manifestName:
		raw, err := io.ReadAll(io.LimitReader(tr, metaMaxBytes))
		if err != nil {
			return nil, false
		}
		if !state.kept.take(len(raw)) {
			return tooMuchKept(), true
		}
		recordManifest(facts, raw)
	}
	return nil, true
}

func judgeTarTables(tables map[tableRef]*tableFacts) (snapshotCensus, *protoError, bool) {
	if len(tables) == 0 {
		return snapshotCensus{}, nil, false
	}
	census := snapshotCensus{}
	for ref, facts := range tables {
		if perr := judgeName("keyspace", ref.keyspace); perr != nil {
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
	sortTables(census.tables)
	return census, nil, true
}

func splitTarName(name string) []string {
	parts := strings.Split(strings.Trim(filepath.ToSlash(name), "/"), "/")
	out := parts[:0]
	for _, p := range parts {
		if p != "" && p != "." {
			out = append(out, p)
		}
	}
	return out
}
