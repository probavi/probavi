package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// aof.go reads what a Valkey append-only directory states about itself.
// The fork kept Redis 7's layout: the append-only state is a directory
// (the server's appenddirname, "appendonlydir" by default) holding a
// text manifest plus the files it names — one base (an RDB by default)
// and incremental AOF segments. The manifest is the authority on what
// the artifact must contain, which gives this adapter the gate AOF
// backups need most: a copy taken mid-rewrite loses members, and a
// manifest naming a file the backup does not hold is positive evidence
// of exactly that — refused by name, before a byte reaches the sandbox.
//
// The base file, when it is an RDB, carries the same header the rdb
// kinds read (rdbmeta.go): valkey-ver feeds the engine-version
// pre-check and the Redis-dialect markers feed the fence. Its ctime is
// deliberately NOT reported as created_at — it dates the last rewrite,
// not the backup, and the incremental tail extends past it; an
// append-only directory does not date itself, so the field stays null
// rather than claiming a wrong instant.

const (
	// aofManifestSuffix marks the manifest file: the server names it
	// "<appendfilename>.manifest".
	aofManifestSuffix = ".manifest"
	// aofManifestMaxBytes bounds the manifest read: real ones are a few
	// lines (measured).
	aofManifestMaxBytes = 1 << 20
)

// aofArtifact is a resolved append-only directory: the manifest plus
// every file it names.
type aofArtifact struct {
	// dir is the host path of the append-only directory.
	dir string
	// manifestName is the manifest's basename; the appendfilename the
	// restored server must be handed derives from it.
	manifestName string
	// files are the basenames the manifest names, in manifest order.
	files []string
	// baseName is the basename of the type-b entry, "" when the
	// manifest names none (an incr-only set replays from empty).
	baseName string
	// incrNames are the type-i entries, manifest order: the segments the
	// server replays. History entries (type h) are members of the set —
	// transferred and checksummed — but the server never loads them, so
	// the integrity gate does not vet them either.
	incrNames []string
}

// appendFilename is the --appendfilename the restored server needs so
// it reads this manifest instead of silently starting a fresh, empty
// append-only set — the false green an unmatched name would produce.
func (a *aofArtifact) appendFilename() string {
	return strings.TrimSuffix(a.manifestName, aofManifestSuffix)
}

// transferNames lists every file to place in the sandbox: the manifest
// first, then the files it names.
func (a *aofArtifact) transferNames() []string {
	return append([]string{a.manifestName}, a.files...)
}

// resolveAOFDir vets an append-only directory by what its own manifest
// states.
func resolveAOFDir(path string) (*aofArtifact, *protoError) {
	entries, perr := readAOFDir(path)
	if perr != nil {
		return nil, perr
	}
	manifestName, perr := findManifest(path, entries)
	if perr != nil {
		return nil, perr
	}
	art, perr := parseAOFManifest(path, manifestName)
	if perr != nil {
		return nil, perr
	}
	present := map[string]bool{}
	for _, e := range entries {
		if e.Type().IsRegular() {
			present[e.Name()] = true
		}
	}
	for _, name := range art.files {
		if !present[name] {
			return nil, protoErr("source_corrupt", false,
				"the manifest names %s, which the backup does not contain — the copy is incomplete "+
					"(a rewrite replaces the append-only set's members, so the whole directory must be "+
					"captured atomically; copy it again while rewrites are not running)", name)
		}
	}
	return art, nil
}

func readAOFDir(path string) ([]os.DirEntry, *protoError) {
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup source does not exist: %s", path)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup source: %v", err)
	case !info.IsDir():
		return nil, protoErr("invalid_request", false,
			"source path %s is a file; valkey_aof restores the append-only directory Valkey kept "+
				"from Redis 7 (manifest, base, incremental segments) — a single-file AOF is not "+
				"supported, see the adapter README", path)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	return entries, nil
}

// findManifest locates the one manifest an append-only directory holds.
// A directory without one is not that artifact — and when a
// subdirectory holds one, the operator pointed one level too high (a
// data-directory copy), which the refusal says in terms of the fix.
func findManifest(dir string, entries []os.DirEntry) (string, *protoError) {
	names := []string{}
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), aofManifestSuffix) {
			names = append(names, e.Name())
		}
	}
	switch {
	case len(names) == 1:
		return names[0], nil
	case len(names) > 1:
		sort.Strings(names)
		return "", protoErr("source_corrupt", false,
			"backup directory holds %d manifest files (%s) — an append-only directory has exactly one",
			len(names), strings.Join(names, ", "))
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if sub, err := os.ReadDir(filepath.Join(dir, e.Name())); err == nil {
			for _, s := range sub {
				if s.Type().IsRegular() && strings.HasSuffix(s.Name(), aofManifestSuffix) {
					return "", protoErr("invalid_request", false,
						"%s looks like a data-directory copy: the append-only directory is %s — "+
							"point source.path there", dir, filepath.Join(dir, e.Name()))
				}
			}
		}
	}
	return "", protoErr("source_corrupt", false,
		"backup directory %s holds no .manifest file — not an append-only directory "+
			"(a copy of appendonlydir holds the manifest, its base file, and incremental segments)", dir)
}

// parseAOFManifest reads the manifest's file list. Each line is
// key-value pairs — "file <name> seq <n> type <b|h|i>" (measured) — and
// a manifest that exists but cannot be read strictly is damage, not a
// bonus miss: the file list is the restore's contract.
func parseAOFManifest(dir, manifestName string) (*aofArtifact, *protoError) {
	raw, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		return nil, protoErr("source_unreadable", false, "read manifest: %v", err)
	}
	if len(raw) > aofManifestMaxBytes {
		return nil, protoErr("source_corrupt", false,
			"manifest %s is %d bytes — no real append-only manifest approaches this", manifestName, len(raw))
	}
	art := &aofArtifact{dir: dir, manifestName: manifestName}
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, typ, ok := parseManifestLine(line)
		if !ok {
			return nil, protoErr("source_corrupt", false,
				"manifest line %d is not a file entry — the manifest is damaged, or this is not "+
					"an append-only manifest", i+1)
		}
		art.files = append(art.files, name)
		switch typ {
		case "b":
			if art.baseName != "" {
				return nil, protoErr("source_corrupt", false,
					"the manifest names two base files (%s, %s) — a real append-only set has at most one",
					art.baseName, name)
			}
			art.baseName = name
		case "i":
			art.incrNames = append(art.incrNames, name)
		}
	}
	if len(art.files) == 0 {
		return nil, protoErr("source_corrupt", false, "manifest %s names no files", manifestName)
	}
	return art, nil
}

// parseManifestLine reads one manifest entry's key-value pairs. The
// server quotes a filename only when it needs escaping; such names are
// refused with the parse rather than guessed at.
func parseManifestLine(line string) (name, typ string, ok bool) {
	fields := strings.Fields(line)
	if len(fields)%2 != 0 {
		return "", "", false
	}
	for i := 0; i < len(fields); i += 2 {
		switch fields[i] {
		case "file":
			name = fields[i+1]
		case "type":
			typ = fields[i+1]
		}
	}
	if name == "" || typ == "" || strings.HasPrefix(name, `"`) {
		return "", "", false
	}
	return name, typ, true
}

// aofChecksum hashes the artifact canonically: the manifest plus every
// named file, sorted by name; each contributes name, size, and content
// bytes. The same set always hashes the same, any member change changes
// the hash. The hash feeds the evidence record's backup identity, so it
// must be a real measurement of the bytes that will be restored — stray
// files beside the set (checksum sidecars, READMEs) are not part of it.
func aofChecksum(art *aofArtifact) (checksum string, sizeBytes int64, perr *protoError) {
	names := append([]string{}, art.transferNames()...)
	sort.Strings(names)
	h := sha256.New()
	var total int64
	for _, name := range names {
		path := filepath.Join(art.dir, name)
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
