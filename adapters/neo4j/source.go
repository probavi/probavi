package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// resolvedSource is a concrete backup artifact chosen for restore.
type resolvedSource struct {
	path      string
	checksum  string // "sha256:<hex>" over the artifact bytes
	sizeBytes int64
	// createdAt is always nil for this adapter: a Neo4j dump records no
	// backup timestamp, and a file's mtime dates a copy rather than a
	// backup (see zone.go).
	createdAt *string
}

// resolveSource maps a source kind to one restorable artifact.
//
//	neo4j_dump     — path is one `neo4j-admin database dump` file
//	neo4j_dump_dir — path is a directory; the newest regular file is chosen
//
// The artifact's own file name never matters: `neo4j-admin database load`
// derives the file name from the database name, so the adapter places
// whatever it was given as <database>.dump inside the sandbox. A backup
// job may therefore name its output whatever it likes.
func resolveSource(ctx context.Context, kind, path string,
	params map[string]string) (*resolvedSource, *protoError) {
	policy, perr := backupSelection(kind, params)
	if perr != nil {
		return nil, perr
	}
	switch kind {
	case "neo4j_dump":
		return resolveFile(path)
	case "neo4j_dump_dir":
		chosen, perr := chooseDumpIn(ctx, path, policy)
		if perr != nil {
			return nil, perr
		}
		return resolveFile(chosen)
	default:
		return nil, protoErr("unsupported_source", false,
			"unsupported source kind: %s (supported: neo4j_dump, neo4j_dump_dir)", kind)
	}
}

func resolveFile(path string) (*resolvedSource, *protoError) {
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup source does not exist: %s", path)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup source: %v", err)
	case info.IsDir():
		return nil, protoErr("invalid_request", false,
			"source path %s is a directory; use kind neo4j_dump_dir for directories", path)
	}
	checksum, perr := fileChecksum(path)
	if perr != nil {
		return nil, perr
	}
	return &resolvedSource{path: path, checksum: checksum, sizeBytes: info.Size()}, nil
}

// chooseDumpIn picks the dump the policy asks for, ordered by file time
// (selection.go says why that is all there is).
func chooseDumpIn(ctx context.Context, dir string, policy selectPolicy) (string, *protoError) {
	entries, err := os.ReadDir(dir)
	switch {
	case os.IsNotExist(err):
		return "", protoErr("source_not_found", false, "backup directory does not exist: %s", dir)
	case err != nil:
		return "", protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	candidates := make([]dirCandidate, 0, len(entries))
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return "", protoErr("source_unreadable", false, "stat %s: %v", e.Name(), err)
		}
		candidates = append(candidates, dirCandidate{
			path: filepath.Join(dir, e.Name()), name: e.Name(), mtime: info.ModTime(),
		})
	}
	if len(candidates) == 0 {
		return "", protoErr("source_not_found", false, "backup directory %s contains no files", dir)
	}
	chosen := pick(candidates, policy).path
	// The adapter chose this file, not the operator — under every policy:
	// make sure a backup job is not still writing it (see settle.go).
	if perr := assertSettled(ctx, chosen, settleWindow); perr != nil {
		return "", perr
	}
	return chosen, nil
}

// fileChecksum streams the artifact once. The hash feeds the evidence
// record's backup identity, so it must be a real measurement of the bytes
// that will be restored — never of a name, a size, or a header.
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
