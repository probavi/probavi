package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// source.go reads what an `influx backup` directory states about itself,
// out of its own manifest. A 2.x backup is a directory of files sharing
// one UTC timestamp stem — <stem>.manifest (JSON), <stem>.bolt.gz (the
// KV metadata store), <stem>.sqlite.gz, and one <stem>.<shard>.tar.gz
// per shard (all measured against 2.7.12). Three facts follow: the
// manifest is the authority on what the artifact must contain, which
// gives the completeness gate a partial copy fails by name; the stem
// dates the backup exactly, with no timezone question; and the
// manifest's own shape separates a 2.x backup from the 1.x portable
// format (a top-level meta entry, no kv — measured), whose restore into
// 2.x is a migration, not a recovery.

const (
	manifestSuffix = ".manifest"
	// manifestMaxBytes bounds the manifest read; real manifests are a few
	// KB (measured).
	manifestMaxBytes = 8 << 20
	// keptMaxBytes and keptMaxEntries bound what one archive walk holds
	// on to across entries: member names, and the manifest bodies read
	// out of the stream at up to manifestMaxBytes each. A tar entry is a
	// 512-byte header that compresses to almost nothing, so a small
	// archive can carry any number of them, and a backup file is
	// attacker-controlled input (SECURITY.md). The entry bound is set
	// against a real backup's shape rather than against a round number:
	// a backup directory holds one file per shard beside the KV, SQL and
	// manifest files, and a busy instance with years of retention still
	// counts in the tens of thousands.
	keptMaxBytes   = 64 << 20
	keptMaxEntries = 200_000
	// manifestVersionVerified is the only manifest format this adapter is
	// verified against (measured on 2.7.12 through 2.9.1).
	manifestVersionVerified = 2
	// stemLayout parses the backup instant out of the shared filename
	// stem ("20260817T194144Z", measured).
	stemLayout = "20060102T150405Z"
)

// resolvedSource is a concrete backup artifact chosen for restore.
type resolvedSource struct {
	// dir is the host path of the backup directory (or of the archive,
	// for the tar kind).
	dir string
	// manifestName is the chosen manifest's basename; files are the
	// members it names plus the manifest itself, in transfer order
	// (empty for the tar kind, which transfers the archive whole).
	manifestName string
	files        []string
	checksum     string // "sha256:<hex>" over the canonical member set (or archive bytes)
	sizeBytes    int64
	// tarball reports the archive kind: unpack in the sandbox, and when
	// orgs is empty recover the manifest from the unpacked tree.
	tarball bool
	// createdAt is the backup instant the filename stem states; nil when
	// the stem does not parse.
	createdAt *string
	// orgs maps each organization the manifest names to its bucket
	// names — the census the restore must satisfy, and the source of the
	// connection's organization. Empty for an archive the host could not
	// walk; ops.go then recovers the same facts from the unpacked tree.
	orgs map[string][]string
}

// backupManifest is the slice of a 2.x manifest this adapter reads —
// plus the meta entry only the 1.x portable format writes, parsed so
// that artifact can be refused by name instead of failing as corrupt.
type backupManifest struct {
	ManifestVersion *int             `json:"manifestVersion"`
	KV              *manifestFile    `json:"kv"`
	SQL             *manifestFile    `json:"sql"`
	Meta            *manifestFile    `json:"meta"`
	Buckets         []manifestBucket `json:"buckets"`
}

type manifestFile struct {
	FileName string `json:"fileName"`
	Size     int64  `json:"size"`
}

type manifestBucket struct {
	OrganizationName  string `json:"organizationName"`
	BucketName        string `json:"bucketName"`
	RetentionPolicies []struct {
		ShardGroups []struct {
			Shards []struct {
				FileName string `json:"fileName"`
			} `json:"shards"`
		} `json:"shardGroups"`
	} `json:"retentionPolicies"`
}

// resolveSource maps a source kind to one restorable artifact.
//
//	influx_backup_tar — path is one tar archive (plain or gzip) of an
//	                    `influx backup` directory, members at the root
//	                    or under one wrapping directory
//	influx_backup     — path is one `influx backup` output directory
//	influx_backup_dir — path is a directory of them; the newest by the
//	                    backups' own timestamp stems is restored
func resolveSource(kind, path string) (*resolvedSource, *protoError) {
	switch kind {
	case "influx_backup_tar":
		return resolveTar(path)
	case "influx_backup":
		return resolveBackupDir(path)
	case "influx_backup_dir":
		latest, perr := newestBackupIn(path)
		if perr != nil {
			return nil, perr
		}
		return resolveBackupDir(latest)
	default:
		return nil, protoErr("unsupported_source", false,
			"unsupported source kind: %s (supported: influx_backup_tar, influx_backup, influx_backup_dir)", kind)
	}
}

// resolveTar vets an archive artifact with what the host can read out of
// it. The tar listing is a bonus — an archive the host cannot walk still
// resolves, and the sandbox extraction plus `influx restore` become the
// authority — except where an entry is positive evidence: a 1.x portable
// manifest, an unverified manifest version, or a member the manifest
// names and the archive does not hold.
func resolveTar(path string) (*resolvedSource, *protoError) {
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup source does not exist: %s", path)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup source: %v", err)
	case info.IsDir():
		return nil, protoErr("invalid_request", false,
			"source path %s is a directory; use kind influx_backup for a backup directory, or "+
				"influx_backup_dir for a directory of them", path)
	}
	src := &resolvedSource{dir: path, tarball: true, sizeBytes: info.Size()}
	manifests, members, perr, ok := listTarBackup(path)
	if perr != nil {
		return nil, perr
	}
	if ok {
		name, perr := chooseTarManifest(manifests)
		if perr != nil {
			return nil, perr
		}
		m, perr := parseManifestBytes(manifests[name], name)
		if perr != nil {
			return nil, perr
		}
		if perr := tarCompleteness(m, members); perr != nil {
			return nil, perr
		}
		src.manifestName = name
		src.orgs = orgsOf(m)
		src.createdAt = stemInstant(name)
	}
	checksum, perr := fileChecksum(path)
	if perr != nil {
		return nil, perr
	}
	src.checksum = checksum
	return src, nil
}

// listTarBackup walks the archive without unpacking it: manifest
// contents and the member-name set, both at the root or under one
// wrapping directory (the two layouts a tar of a backup directory
// takes). ok is false for an archive Go's readers cannot walk —
// metadata is a bonus, never a verdict.
func listTarBackup(path string) (manifests map[string][]byte, members map[string]bool, perr *protoError, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, nil, false
	}
	defer f.Close() //nolint:errcheck // read-only walk; the checksum pass reopens it
	var r io.Reader = f
	head := make([]byte, 2)
	if _, err := io.ReadFull(f, head); err != nil {
		return nil, nil, nil, false
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, nil, nil, false
	}
	if head[0] == 0x1f && head[1] == 0x8b {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, nil, nil, false
		}
		r = gz
	}
	tr := tar.NewReader(r)
	manifests = map[string][]byte{}
	members = map[string]bool{}
	kept := retention{}
	sawEntry := false
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, nil, false
		}
		sawEntry = true
		switch recordTarEntry(tr, hdr, manifests, members, &kept) {
		case entryRecorded:
		case entryUnreadable:
			return nil, nil, nil, false
		case entryOverBudget:
			return nil, nil, tooMuchKept(), false
		}
	}
	return manifests, members, nil, sawEntry
}

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

// tooMuchKept refuses an archive whose bookkeeping this walk cannot
// bound. It is a verdict rather than a silent skip: the listing is a
// bonus, but an archive built to exhaust the drill host's memory is
// positive evidence about the source.
func tooMuchKept() *protoError {
	return protoErr("source_corrupt", false,
		"the archive carries more members than an `influx backup` directory holds — over %d of them, "+
			"or more than %d MiB of manifests and names. Reading on would cost the drill host memory "+
			"an archive gets to choose, so the walk stops here",
		keptMaxEntries, keptMaxBytes>>20)
}

// entryVerdict is what one archive entry did to the walk.
type entryVerdict int

const (
	entryRecorded entryVerdict = iota
	entryUnreadable
	entryOverBudget
)

// recordTarEntry notes one archive entry: member names at the accepted
// depths, and manifest contents read out of the stream. False means the
// walk itself must give up.
func recordTarEntry(tr *tar.Reader, hdr *tar.Header, manifests map[string][]byte,
	members map[string]bool, kept *retention) entryVerdict {
	if hdr.Typeflag != tar.TypeReg {
		return entryRecorded
	}
	segments := strings.Split(strings.TrimPrefix(strings.TrimSuffix(hdr.Name, "/"), "./"), "/")
	if len(segments) < 1 || len(segments) > 2 {
		return entryRecorded
	}
	name := segments[len(segments)-1]
	if !members[name] && !kept.take(len(name)) {
		return entryOverBudget
	}
	members[name] = true
	if strings.HasSuffix(name, manifestSuffix) {
		raw, err := io.ReadAll(io.LimitReader(tr, manifestMaxBytes+1))
		if err != nil || len(raw) > manifestMaxBytes {
			return entryUnreadable
		}
		if _, seen := manifests[name]; !seen && !kept.take(len(raw)) {
			return entryOverBudget
		}
		manifests[name] = raw
	}
	return entryRecorded
}

// chooseTarManifest mirrors chooseManifest for entries read out of an
// archive.
func chooseTarManifest(manifests map[string][]byte) (string, *protoError) {
	names := make([]string, 0, len(manifests))
	for name := range manifests {
		names = append(names, name)
	}
	sort.Strings(names)
	switch len(names) {
	case 0:
		return "", protoErr("source_corrupt", false,
			"the archive holds no .manifest file — not a tar of an `influx backup` directory")
	case 1:
		return names[0], nil
	}
	best := ""
	var bestTS time.Time
	for _, name := range names {
		ts, err := time.Parse(stemLayout, strings.TrimSuffix(name, manifestSuffix))
		if err != nil {
			return "", protoErr("source_corrupt", false,
				"the archive holds %d manifests (%s) and %s does not carry the timestamp stem "+
					"`influx backup` writes — which backup to restore cannot be decided honestly",
				len(names), strings.Join(names, ", "), name)
		}
		if best == "" || ts.After(bestTS) || (ts.Equal(bestTS) && name > best) {
			best, bestTS = name, ts
		}
	}
	return best, nil
}

// tarCompleteness refuses an archive whose chosen manifest names a
// member the archive does not hold — the same incomplete-copy gate the
// directory kind has.
func tarCompleteness(m *backupManifest, members map[string]bool) *protoError {
	for _, name := range manifestMemberNames(m) {
		if !members[name] {
			return protoErr("source_corrupt", false,
				"the manifest names %s, which the archive does not contain — the copy is incomplete; "+
					"tar the whole backup directory", name)
		}
	}
	return nil
}

// orgsOf collects the manifest's organizations and their bucket names.
func orgsOf(m *backupManifest) map[string][]string {
	orgs := map[string][]string{}
	for _, b := range m.Buckets {
		orgs[b.OrganizationName] = append(orgs[b.OrganizationName], b.BucketName)
	}
	return orgs
}

// stemInstant renders the backup instant a manifest name states, or nil
// when the stem does not parse.
func stemInstant(manifestName string) *string {
	ts, err := time.Parse(stemLayout, strings.TrimSuffix(manifestName, manifestSuffix))
	if err != nil {
		return nil
	}
	s := ts.UTC().Format("2006-01-02T15:04:05.000Z07:00")
	return &s
}

// fileChecksum streams the artifact once. The hash feeds the evidence
// record's backup identity, so it must be a real measurement of the
// bytes that will be restored.
func fileChecksum(path string) (string, *protoError) {
	f, err := os.Open(path)
	if err != nil {
		return "", protoErr("source_unreadable", false, "open backup source: %v", err)
	}
	h := sha256.New()
	_, cerr := io.Copy(h, f)
	if err := f.Close(); err != nil && cerr == nil {
		cerr = err
	}
	if cerr != nil {
		return "", protoErr("source_unreadable", false, "read backup source: %v", cerr)
	}
	return fmt.Sprintf("sha256:%s", hex.EncodeToString(h.Sum(nil))), nil
}

// resolveBackupDir vets one backup directory by what its own manifest
// states.
func resolveBackupDir(path string) (*resolvedSource, *protoError) {
	entries, perr := readBackupDir(path)
	if perr != nil {
		return nil, perr
	}
	manifestName, perr := chooseManifest(path, entries)
	if perr != nil {
		return nil, perr
	}
	m, perr := parseManifest(path, manifestName)
	if perr != nil {
		return nil, perr
	}
	src := &resolvedSource{dir: path, manifestName: manifestName}
	src.files, perr = memberFiles(m, manifestName, entries)
	if perr != nil {
		return nil, perr
	}
	src.orgs = orgsOf(m)
	src.createdAt = stemInstant(manifestName)
	src.checksum, src.sizeBytes, perr = memberChecksum(path, src.files)
	if perr != nil {
		return nil, perr
	}
	return src, nil
}

func readBackupDir(path string) ([]os.DirEntry, *protoError) {
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup source does not exist: %s", path)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup source: %v", err)
	case !info.IsDir():
		return nil, protoErr("invalid_request", false,
			"source path %s is a file; influx_backup restores the directory `influx backup` writes "+
				"(a timestamped .manifest beside the KV, SQL, and shard files)", path)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	return entries, nil
}

// chooseManifest picks the backup to restore when the directory holds
// several: `influx backup` into a reused target directory accumulates
// timestamped sets side by side, and the drill restores the newest by
// the stems the backups named themselves — never file times a copy
// would reset. Several manifests with any unparsable stem is ambiguity
// this adapter refuses to guess about.
func chooseManifest(dir string, entries []os.DirEntry) (string, *protoError) {
	names := []string{}
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), manifestSuffix) {
			names = append(names, e.Name())
		}
	}
	switch len(names) {
	case 0:
		return "", protoErr("source_corrupt", false,
			"backup directory %s holds no .manifest file — not an `influx backup` directory "+
				"(the CLI writes a timestamped manifest beside the KV, SQL, and shard files)", dir)
	case 1:
		return names[0], nil
	}
	type dated struct {
		name string
		ts   time.Time
	}
	backups := make([]dated, 0, len(names))
	for _, name := range names {
		ts, err := time.Parse(stemLayout, strings.TrimSuffix(name, manifestSuffix))
		if err != nil {
			sort.Strings(names)
			return "", protoErr("source_corrupt", false,
				"backup directory holds %d manifests (%s) and %s does not carry the timestamp stem "+
					"`influx backup` writes — which backup to restore cannot be decided honestly",
				len(names), strings.Join(names, ", "), name)
		}
		backups = append(backups, dated{name: name, ts: ts})
	}
	sort.Slice(backups, func(i, j int) bool {
		if !backups[i].ts.Equal(backups[j].ts) {
			return backups[i].ts.After(backups[j].ts)
		}
		return backups[i].name > backups[j].name
	})
	return backups[0].name, nil
}

// parseManifest reads the chosen manifest file and hands it to
// parseManifestBytes.
func parseManifest(dir, manifestName string) (*backupManifest, *protoError) {
	raw, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		return nil, protoErr("source_unreadable", false, "read manifest: %v", err)
	}
	if len(raw) > manifestMaxBytes {
		return nil, protoErr("source_corrupt", false,
			"manifest %s is %d bytes — no real backup manifest approaches this", manifestName, len(raw))
	}
	return parseManifestBytes(raw, manifestName)
}

// parseManifestBytes reads a manifest and refuses the shapes this
// adapter must not restore: the 1.x portable format (a migration away
// from 2.x, not a restore), and manifest versions nothing here has been
// verified against.
func parseManifestBytes(raw []byte, manifestName string) (*backupManifest, *protoError) {
	m := &backupManifest{}
	if err := json.Unmarshal(raw, m); err != nil {
		return nil, protoErr("source_corrupt", false,
			"manifest %s is not valid JSON — the backup is damaged, or this is not an "+
				"`influx backup` directory: %v", manifestName, err)
	}
	if m.Meta != nil && m.KV == nil {
		return nil, protoErr("unsupported_source", false,
			"the manifest is an InfluxDB 1.x portable backup (its meta entry says so): restoring it "+
				"into InfluxDB 2.x is a migration (influxd upgrade), not a recovery, and a drill that "+
				"ran one would prove a path nobody's recovery takes — back up the 2.x instance with "+
				"`influx backup` instead")
	}
	if m.ManifestVersion != nil && *m.ManifestVersion != manifestVersionVerified {
		return nil, protoErr("unsupported_source", false,
			"manifest version %d — this adapter is verified against version %d manifests only",
			*m.ManifestVersion, manifestVersionVerified)
	}
	if m.KV == nil || m.KV.FileName == "" {
		return nil, protoErr("source_corrupt", false,
			"manifest %s names no KV store file — not a complete `influx backup` manifest", manifestName)
	}
	if len(m.Buckets) == 0 {
		return nil, protoErr("source_corrupt", false,
			"manifest %s names no buckets — there is nothing this backup could prove restorable", manifestName)
	}
	return m, nil
}

// manifestMemberNames lists the members a manifest names — the KV
// store, the SQL store when present, every shard — in manifest order,
// without duplicates.
func manifestMemberNames(m *backupManifest) []string {
	names := []string{}
	seen := map[string]bool{}
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	add(m.KV.FileName)
	if m.SQL != nil {
		add(m.SQL.FileName)
	}
	for _, b := range m.Buckets {
		for _, rp := range b.RetentionPolicies {
			for _, sg := range rp.ShardGroups {
				for _, s := range sg.Shards {
					add(s.FileName)
				}
			}
		}
	}
	return names
}

// memberFiles resolves the manifest's members against the directory and
// refuses the one the backup does not hold: a partial copy is the
// artifact's primary real-world failure mode, and restoring the rest
// would prove only what survived.
func memberFiles(m *backupManifest, manifestName string, entries []os.DirEntry) ([]string, *protoError) {
	present := map[string]bool{}
	for _, e := range entries {
		if e.Type().IsRegular() {
			present[e.Name()] = true
		}
	}
	files := []string{manifestName}
	for _, name := range manifestMemberNames(m) {
		if name == manifestName {
			continue
		}
		if !present[name] {
			return nil, protoErr("source_corrupt", false,
				"the manifest names %s, which the backup does not contain — the copy is incomplete; "+
					"copy the whole backup directory", name)
		}
		files = append(files, name)
	}
	return files, nil
}

// backupCandidate is one subdirectory considered by influx_backup_dir.
type backupCandidate struct {
	path string
	ts   time.Time
}

// newestBackupIn picks the subdirectory whose own newest manifest stem
// is the latest — the artifact dates itself, so file times never rank.
// Subdirectories without a manifest are skipped as non-candidates and
// counted, so an empty verdict says what was passed over.
func newestBackupIn(dir string) (string, *protoError) {
	entries, err := os.ReadDir(dir)
	switch {
	case os.IsNotExist(err):
		return "", protoErr("source_not_found", false, "backup directory does not exist: %s", dir)
	case err != nil:
		return "", protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	var best *backupCandidate
	skipped := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sub := filepath.Join(dir, e.Name())
		ts, ok := newestStemIn(sub)
		if !ok {
			skipped++
			continue
		}
		candidate := backupCandidate{path: sub, ts: ts}
		if best == nil || candidate.beats(*best) {
			c := candidate
			best = &c
		}
	}
	if best == nil {
		if skipped > 0 {
			return "", protoErr("source_not_found", false,
				"backup directory %s holds no `influx backup` outputs (%d subdirectories without a "+
					"timestamped manifest were passed over)", dir, skipped)
		}
		return "", protoErr("source_not_found", false,
			"backup directory %s contains no subdirectories", dir)
	}
	return best.path, nil
}

// newestStemIn reports the newest parsable manifest stem a directory
// holds; ok is false when it holds none.
func newestStemIn(dir string) (time.Time, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return time.Time{}, false
	}
	var newest time.Time
	found := false
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), manifestSuffix) {
			continue
		}
		if ts, err := time.Parse(stemLayout, strings.TrimSuffix(e.Name(), manifestSuffix)); err == nil {
			if !found || ts.After(newest) {
				newest = ts
				found = true
			}
		}
	}
	return newest, found
}

// beats orders candidates: a newer own-stated instant wins, ties break
// toward the lexicographically larger name so the choice is
// deterministic.
func (c backupCandidate) beats(o backupCandidate) bool {
	if !c.ts.Equal(o.ts) {
		return c.ts.After(o.ts)
	}
	return filepath.Base(c.path) > filepath.Base(o.path)
}

// memberChecksum hashes the artifact canonically: the manifest plus
// every named member, sorted by name; each contributes name, size, and
// content bytes. The same set always hashes the same, any member change
// changes the hash, and stray files beside the set (checksum sidecars,
// a neighbouring backup's members) are not part of it. The hash feeds
// the evidence record's backup identity, so it must be a real
// measurement of the bytes that will be restored.
func memberChecksum(dir string, files []string) (checksum string, sizeBytes int64, perr *protoError) {
	names := append([]string{}, files...)
	sort.Strings(names)
	h := sha256.New()
	var total int64
	for _, name := range names {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil {
			return "", 0, protoErr("source_unreadable", false, "stat %s: %v", name, err)
		}
		fmt.Fprintf(h, "%s\x00%d\x00", name, info.Size())
		f, err := os.Open(path)
		if err != nil {
			return "", 0, protoErr("source_unreadable", false, "open %s: %v", name, err)
		}
		_, cerr := io.Copy(h, f)
		if err := f.Close(); err != nil && cerr == nil {
			cerr = err
		}
		if cerr != nil {
			return "", 0, protoErr("source_unreadable", false, "read %s: %v", name, cerr)
		}
		total += info.Size()
	}
	return fmt.Sprintf("sha256:%s", hex.EncodeToString(h.Sum(nil))), total, nil
}

// backupTimezoneParam names the IANA zone the backup host was in. The
// wall-clock formats sibling adapters read need it; an `influx backup`
// stem is UTC by construction (the Z suffix the CLI writes), so the
// declaration has nothing to add and is refused rather than ignored.
const backupTimezoneParam = "backup_timezone"

func rejectBackupTimezone(params map[string]string) *protoError {
	if params[backupTimezoneParam] == "" {
		return nil
	}
	return protoErr("invalid_request", false,
		"source.params.%s has no effect for this adapter: a backup's timestamp stem is UTC by "+
			"construction, so backup.created_at is exact without a declared zone — remove the parameter",
		backupTimezoneParam)
}
