package main

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// fence.go answers the question the adapter-development rules ask of
// every engine: what does this one delete on its own, and can a drill
// stop it?
//
// Solr's answer is document expiration. DocExpirationUpdateProcessorFactory
// runs a deleter on a timer and removes every document whose expiry field
// is in the past. It is not in any configset the official image ships —
// but a backup carries the collection's own configset, and a restore
// installs it, so a backup made by an operator who uses expiry brings the
// deleter along with it.
//
// Measured end to end on Solr 10, and the result is the reason this file
// exists rather than a comment somewhere:
//
//	backup taken while 3 documents were live      numFound 3, status 0
//	restored after their expiry had passed        status 0 — success
//	the restored collection, seconds later        numFound 0
//
// The restore reports success and the collection then empties itself. A
// drill would call that green and prove nothing, or fail a count check
// and blame a backup that is perfectly intact.
//
// Nothing can be suspended. The setting lives in the operator's own
// collection config, and rewriting a user's configuration to make a drill
// pass is exactly what "suspend, never rewrite" forbids — a check reading
// the policy must still see what the operator declared. So this adapter
// fences: it refuses the drill, and says why.
//
// The refusal happens host-side, before a byte is transferred, because
// the configset is a file in the artifact the drill was pointed at.

// expirationClass is the processor that deletes on a timer. Solr accepts
// it written either way in solrconfig.xml.
const expirationClass = "DocExpirationUpdateProcessorFactory"

// solrConfigFile is the per-configset file a backup carries.
const solrConfigFile = "solrconfig.xml"

// expiringConfigs returns the configset paths inside artifact whose
// solrconfig.xml enables document expiration, relative to the artifact
// and sorted. Empty means the artifact carries no such configuration.
//
// The walk is scoped to the artifact and reads regular files only, which
// is what the archive pass over the same tree reads (takeTarEntry skips
// every entry that is not tar.TypeReg). A backup is attacker-shaped
// input (SECURITY.md): before this scoping, a solrconfig.xml that was a
// symlink had this pass read whatever it pointed at, anywhere on the
// drill host, while the same tree handed over as a tar was already
// ignored. What is read is bounded the way the archive pass bounds it.
func expiringConfigs(artifact string) ([]string, error) {
	fi, err := os.Lstat(artifact)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		// An archive. inspectBackupTar reads that one, and a path that
		// is neither is reported by the caller that opens it.
		return nil, nil
	}
	// Between the Lstat and the open the artifact may be replaced; that
	// costs nothing here, because what the open yields is either a
	// directory every later name is resolved inside of, or an error.
	root, err := os.OpenRoot(artifact)
	if err != nil {
		return nil, err
	}
	defer root.Close() //nolint:errcheck // read-only walk
	var found []string
	err = fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() || d.Name() != solrConfigFile {
			return nil
		}
		expiring, rerr := configExpires(root, path)
		if rerr != nil {
			return rerr
		}
		if expiring {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(found)
	return found, nil
}

// configExpires reports whether one configuration file enables the
// expiration processor, reading at most maxConfigBytes of it — the bound
// the archive pass applies to the same file.
func configExpires(root *os.Root, name string) (bool, error) {
	f, err := root.Open(name)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", name, err)
	}
	defer f.Close() //nolint:errcheck // read-only descriptor
	raw, err := io.ReadAll(io.LimitReader(f, maxConfigBytes))
	if err != nil {
		return false, fmt.Errorf("read %s: %w", name, err)
	}
	return bytes.Contains(raw, []byte(expirationClass)), nil
}

// rejectExpiringBackup refuses an artifact whose own configuration would
// delete the documents it restores.
func rejectExpiringBackup(artifact string) *protoError {
	found, err := expiringConfigs(artifact)
	if err != nil {
		return protoErr("source_unreadable", false, "read the backup's configuration: %v", err)
	}
	if len(found) == 0 {
		return nil
	}
	return expirationRefusal(found)
}

// expirationRefusal is the one refusal, wherever the configuration was
// found — in a directory or inside an archive.
func expirationRefusal(found []string) *protoError {
	return protoErr("unsupported_source", false,
		"this backup's own configuration enables %s (%s), which deletes every document whose expiry "+
			"field has passed — measured on Solr 10, a restore of such a backup reports success and "+
			"the collection is empty seconds later. The drill is refused rather than reported green: "+
			"the setting belongs to your collection, and an adapter that rewrote it to make a drill "+
			"pass would be proving something other than your backup",
		expirationClass, strings.Join(found, ", "))
}

// inspectBackupTar reads an archive once, reporting the collection it
// holds and any configset inside it that would delete the documents it
// restores.
//
// A stream that is not tar-shaped is not a verdict: the sandbox
// extraction will say so, and this pass falls silent rather than
// inventing a reason. What it must never do is stay silent about
// expiration in an archive it *could* read — the fence is the whole
// reason the pass exists.
func inspectBackupTar(path string) (collection string, expiring []string, perr *protoError) {
	f, err := os.Open(path) //#nosec G304 -- the artifact the drill named.
	if err != nil {
		return "", nil, protoErr("source_unreadable", false, "open backup source: %v", err)
	}
	collection, expiring, perr = scanBackupTar(f, path)
	if cerr := f.Close(); cerr != nil && perr == nil {
		return "", nil, protoErr("source_unreadable", false, "close backup source: %v", cerr)
	}
	return collection, expiring, perr
}

// scanBackupTar is the streaming pass itself, over an open archive.
func scanBackupTar(r io.Reader, path string) (collection string, expiring []string, perr *protoError) {
	tr := tar.NewReader(r)
	collections := map[string]bool{}
	kept := retention{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// Not tar-shaped, or truncated: say nothing here.
			return collection, expiring, nil
		}
		next, within := takeTarEntry(tr, hdr, collections, expiring, &kept)
		if !within {
			return "", nil, tooMuchKept()
		}
		expiring = next
	}
	sort.Strings(expiring)
	names := make([]string, 0, len(collections))
	for c := range collections {
		names = append(names, c)
	}
	sort.Strings(names)
	switch len(names) {
	case 0:
		return "", expiring, nil
	case 1:
		return names[0], expiring, nil
	default:
		return "", expiring, protoErr("unsupported_source", false,
			"%s holds %d collections (%s); this adapter restores one backup into one collection, so "+
				"point the drill at an archive of a single collection",
			path, len(names), strings.Join(names, ", "))
	}
}

// maxConfigBytes bounds what the fence reads out of one configuration
// file. A solrconfig.xml is kilobytes; anything far larger is not one,
// and an archive must never be able to make this pass allocate freely.
const maxConfigBytes = 4 << 20

const (
	// keptMaxBytes and keptMaxEntries bound what this pass holds on to
	// across entries: collection names, and the names of configuration
	// files carrying the expiration class. A tar entry is a 512-byte
	// header that compresses to almost nothing, so a small archive can
	// carry any number of them, and a backup file is attacker-controlled
	// input (SECURITY.md). The entry bound is tight because what this
	// pass retains is inherently tiny: an archive this adapter restores
	// holds one collection, and its configuration files number in the
	// dozens.
	keptMaxBytes   = 64 << 20
	keptMaxEntries = 4096
)

// retention accounts for what the pass keeps rather than for what it
// reads: an archive may hold any number of entries this pass ignores,
// and refusing those would turn a large legitimate backup into a failed
// drill.
type retention struct {
	entries int
	bytes   int
}

// take accounts for one retained name of n bytes and reports whether the
// pass may keep it.
func (r *retention) take(n int) bool {
	r.entries++
	r.bytes += n
	return r.entries <= keptMaxEntries && r.bytes <= keptMaxBytes
}

// tooMuchKept is the verdict for an archive whose bookkeeping this pass
// cannot bound. The fence stays silent about archives it cannot read,
// but an archive built to exhaust the drill host's memory is positive
// evidence about the source.
func tooMuchKept() *protoError {
	return protoErr("source_corrupt", false,
		"the archive carries more collection and configuration names than a Solr backup holds — "+
			"over %d of them, or more than %d MiB. Reading on would cost the drill host memory an "+
			"archive gets to choose, so the walk stops here",
		keptMaxEntries, keptMaxBytes>>20)
}

// collectionOf reports the collection a backup entry belongs to, read
// from the file the engine itself writes beside the index.
// takeTarEntry folds one archive entry into the pass: the collection its
// path names, and configuration files carrying the expiration class.
// within false means the walk's retention budget is spent.
func takeTarEntry(tr *tar.Reader, hdr *tar.Header, collections map[string]bool,
	expiring []string, kept *retention) (next []string, within bool) {
	name := filepath.ToSlash(filepath.Clean(hdr.Name))
	if c := collectionOf(name); c != "" && !collections[c] {
		if !kept.take(len(c)) {
			return expiring, false
		}
		collections[c] = true
	}
	if filepath.Base(name) != solrConfigFile || hdr.Typeflag != tar.TypeReg {
		return expiring, true
	}
	body, rerr := io.ReadAll(io.LimitReader(tr, maxConfigBytes))
	if rerr != nil || !bytes.Contains(body, []byte(expirationClass)) {
		return expiring, true
	}
	if !kept.take(len(name)) {
		return expiring, false
	}
	return append(expiring, name), true
}

func collectionOf(name string) string {
	base := filepath.Base(name)
	if !strings.HasPrefix(base, "backup_") || !strings.HasSuffix(base, ".properties") {
		return ""
	}
	return filepath.Base(filepath.Dir(name))
}
