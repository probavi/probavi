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
	"time"
)

// dumpCensus is what the artifact states about itself.
type dumpCensus struct {
	// database is the database the dump was taken from, as dump.json
	// states it; "" when nothing readable said so.
	database string
	// createdMs is the instant dump.json states, epoch milliseconds; 0
	// when nothing plausible was read.
	createdMs int64
	// collections is every collection the dump carries, sorted.
	collections []string
}

// dumpManifest is the slice of dump.json this adapter reads.
//
// It is thinner than the engine writes, and deliberately: what is not
// used is not claimed. Two fields carry the adapter.
//
//   - database names where the dump came from, and therefore where it is
//     restored — the tool needs a --server.database and this is the only
//     place the artifact says which.
//   - createdAt is RFC 3339 with a literal Z (measured), so
//     backup.created_at is exact and no timezone has to be declared.
//
// What dump.json does *not* carry is a list of the collections, which is
// why completeness here is a pairing check rather than a comparison
// against the artifact's own list (see judgeDump).
type dumpManifest struct {
	Database  string `json:"database"`
	CreatedAt string `json:"createdAt"`
}

// dumpFacts accumulates what one dump directory states about itself.
type dumpFacts struct {
	manifestOK bool
	manifest   dumpManifest
	createdMs  int64
	// encryption is what the ENCRYPTION file says, "" when absent.
	encryption string
	// structures and data are the collection names each half was seen
	// for; a collection needs both.
	structures map[string]bool
	data       map[string]bool
}

func newDumpFacts() *dumpFacts {
	return &dumpFacts{structures: map[string]bool{}, data: map[string]bool{}}
}

// collectionShape matches the two file names arangodump writes per
// collection: <name>_<32 hex>.structure.json and the matching data file.
// The name is captured so the pairing can be checked by collection rather
// than by file.
var collectionShape = regexp.MustCompile(`^(.+)_[0-9a-f]{32}\.(structure\.json|data\.json(\.gz)?)$`)

// identifierShape is the collection or database name this adapter accepts.
// Names reach composed AQL and argv, so anything else is refused rather
// than quoted around.
var identifierShape = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,63}$`)

const (
	manifestName   = "dump.json"
	encryptionName = "ENCRYPTION"
	// encryptionNone is what an unencrypted dump states. Encryption is an
	// Enterprise feature and the file is written either way, so this is
	// how an encrypted artifact is recognised rather than guessed at.
	encryptionNone = "none"
	// metaMaxBytes bounds manifest reads; real ones are a few hundred
	// bytes.
	metaMaxBytes = 1 << 20
	// keptMaxEntries bounds what one archive walk holds on to. A tar
	// entry is a 512-byte header that compresses to almost nothing, and a
	// backup file is attacker-controlled input (SECURITY.md).
	keptMaxEntries = 200_000
)

// judgeDump turns one dump's facts into a verdict.
func judgeDump(facts *dumpFacts) *protoError {
	if facts.encryption != "" && !strings.EqualFold(facts.encryption, encryptionNone) {
		return protoErr("unsupported_source", false,
			"the dump states encryption %q in its %s: this adapter restores unencrypted dumps, "+
				"and decrypting one would need a key a drill must never hold",
			facts.encryption, encryptionName)
	}
	if !facts.manifestOK {
		return protoErr("source_corrupt", false,
			"the dump holds no readable %s — arangodump writes one with every dump, and it is "+
				"what names the database to restore into", manifestName)
	}
	if facts.manifest.Database == "" {
		return protoErr("source_corrupt", false,
			"%s names no database: without it the restore has nowhere to go", manifestName)
	}
	if perr := judgeName("database", facts.manifest.Database); perr != nil {
		return perr
	}
	return judgePairs(facts)
}

// judgePairs holds each collection to both of its halves.
//
// arangodump writes a structure.json and a data file for every
// collection — including one with no documents at all, measured, which is
// what makes this safe to require. So a half-copied dump is detectable:
// a structure with no data is a collection whose contents went missing,
// and data with no structure is one the restore could not recreate.
func judgePairs(facts *dumpFacts) *protoError {
	var missingData, missingStructure []string
	for name := range facts.structures {
		if !facts.data[name] {
			missingData = append(missingData, name)
		}
	}
	for name := range facts.data {
		if !facts.structures[name] {
			missingStructure = append(missingStructure, name)
		}
	}
	sort.Strings(missingData)
	sort.Strings(missingStructure)
	if len(missingData) > 0 {
		return protoErr("source_corrupt", false,
			"collection %s has its definition and no data file: the copy is incomplete, and "+
				"restoring it would report an empty collection as restored",
			strings.Join(trimList(missingData), ", "))
	}
	if len(missingStructure) > 0 {
		return protoErr("source_corrupt", false,
			"collection %s has a data file and no definition: the restore cannot recreate it",
			strings.Join(trimList(missingStructure), ", "))
	}
	if len(facts.structures) == 0 {
		return protoErr("source_corrupt", false,
			"the dump carries no collection at all — arangorestore reports that as a successful "+
				"restore of nothing (measured), so it is refused here instead")
	}
	return nil
}

// trimList keeps a message short; the drill host's log has the rest.
func trimList(names []string) []string {
	if len(names) > 3 {
		return names[:3]
	}
	return names
}

// judgeName refuses a database or collection name that is not usable
// unquoted.
func judgeName(kind, name string) *protoError {
	if !identifierShape.MatchString(name) {
		return protoErr("invalid_request", false,
			"%s name %q is not one this adapter can put into a query or a command line unchanged",
			kind, name)
	}
	return nil
}

// recordFile folds one dump file into the facts.
func recordFile(facts *dumpFacts, name string, read func() ([]byte, error)) error {
	switch name {
	case manifestName:
		raw, err := read()
		if err != nil {
			return err
		}
		recordManifest(facts, raw)
		return nil
	case encryptionName:
		raw, err := read()
		if err != nil {
			return err
		}
		facts.encryption = strings.TrimSpace(string(raw))
		return nil
	}
	m := collectionShape.FindStringSubmatch(name)
	if m == nil {
		return nil
	}
	if strings.HasPrefix(m[2], "structure") {
		facts.structures[m[1]] = true
	} else {
		facts.data[m[1]] = true
	}
	return nil
}

// recordManifest reads dump.json and keeps the instant it states.
func recordManifest(facts *dumpFacts, raw []byte) {
	m := dumpManifest{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return
	}
	facts.manifestOK = true
	facts.manifest = m
	if ts, err := time.Parse(time.RFC3339, m.CreatedAt); err == nil {
		if ms := ts.UTC().UnixMilli(); plausibleEpochMs(ms) {
			facts.createdMs = ms
		}
	}
}

// plausibleEpochMs rejects an instant no dump of a running engine could
// carry, so a corrupt or hostile manifest cannot date a record.
func plausibleEpochMs(ms int64) bool {
	const y2015 = 1420070400000
	const y2100 = 4102444800000
	return ms > y2015 && ms < y2100
}

// censusOf renders the facts as what the artifact states about itself.
func censusOf(facts *dumpFacts) dumpCensus {
	names := make([]string, 0, len(facts.structures))
	for name := range facts.structures {
		names = append(names, name)
	}
	sort.Strings(names)
	return dumpCensus{
		database:    facts.manifest.Database,
		createdMs:   facts.createdMs,
		collections: names,
	}
}

// inspectDumpDir reads an arangodump output directory on the host.
func inspectDumpDir(dir string) (dumpCensus, *protoError) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return dumpCensus{}, protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	facts := newDumpFacts()
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if err := recordFile(facts, name, func() ([]byte, error) {
			return readCapped(filepath.Join(dir, name))
		}); err != nil {
			return dumpCensus{}, protoErr("source_unreadable", false, "read %s: %v", name, err)
		}
	}
	if perr := judgeDump(facts); perr != nil {
		return dumpCensus{}, perr
	}
	return censusOf(facts), nil
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

// inspectDumpTar reads what the host can out of an archive in one
// streaming pass, and falls silent where the stream is not tar-shaped:
// the sandbox extraction is then the authority, because metadata is a
// bonus and verdicts are not guesses.
func inspectDumpTar(path string) (census dumpCensus, verdict *protoError, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return dumpCensus{}, nil, false
	}
	census, verdict, ok = walkTar(tar.NewReader(f))
	if cerr := f.Close(); cerr != nil && verdict == nil {
		return dumpCensus{}, nil, false
	}
	return census, verdict, ok
}

func walkTar(tr *tar.Reader) (dumpCensus, *protoError, bool) {
	facts := newDumpFacts()
	entries := 0
	saw := false
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// Not tar-shaped, or damaged past this point: say nothing and
			// let the sandbox's own tar be the authority.
			return dumpCensus{}, nil, false
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if entries++; entries > keptMaxEntries {
			return dumpCensus{}, protoErr("source_corrupt", false,
				"the archive's metadata exceeds what an arangodump output carries"), true
		}
		base := filepath.Base(filepath.ToSlash(hdr.Name))
		saw = true
		if err := recordFile(facts, base, func() ([]byte, error) {
			return io.ReadAll(io.LimitReader(tr, metaMaxBytes))
		}); err != nil {
			return dumpCensus{}, nil, false
		}
	}
	if !saw {
		return dumpCensus{}, nil, false
	}
	if perr := judgeDump(facts); perr != nil {
		return dumpCensus{}, perr, true
	}
	return censusOf(facts), nil, true
}
