package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Source kinds this adapter accepts.
const (
	// kindData is an IoTDB offline copy: the data directory of a standalone
	// node, copied while the node was stopped — by the vendor's
	// tools/ops/backup.sh, whose target holds a whole installation with the
	// data under data/, or by any copy of the data directory itself.
	kindData = "iotdb_data"
	// kindDataTar is a tar archive of one, plain or gzip-compressed. The
	// compression is read from the bytes, never from the file name.
	kindDataTar = "iotdb_data_tar"
)

// The two files that make a directory an IoTDB data directory: each node's
// record of itself. Both carry the version that wrote them, and the
// addresses and ports the node was configured with — which the engine
// refuses to start without (measured).
const (
	confignodeProperties = "confignode/system/confignode-system.properties"
	datanodeProperties   = "datanode/system/system.properties"
)

// maxDataRootDepth is how deep under the named path a data directory is
// looked for: the backup tool's target puts it one level down, and an
// operator's own wrapping directory adds another.
const maxDataRootDepth = 3

// source is what host-side inspection could establish about the artifact
// before a byte moves into the sandbox.
type source struct {
	// kind is the declared source kind.
	kind string
	// path is the artifact on the drill host: the data directory itself
	// for kindData, wherever under the named path it was found.
	path string
	// checksum is the sha256 reference recorded as the backup's identity.
	checksum string
	// sizeBytes is what the checksum covered.
	sizeBytes int64
	// gzip reports whether an archive's bytes are gzip-compressed.
	gzip bool
}

// inspect reads what the artifact states about itself, refusing what this
// adapter cannot honestly judge.
func inspect(kind, path string) (*source, *protoError) {
	// The kind is judged before the path: an unknown kind is unsupported
	// whether or not anything sits at the path.
	if kind != kindData && kind != kindDataTar {
		return nil, protoErr("unsupported_source", false, "unsupported source kind %s", kind)
	}
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil, protoErr("source_not_found", false, "backup source %s does not exist", path)
	}
	if err != nil {
		return nil, protoErr("source_unreadable", false, "stat backup source: %v", err)
	}
	if kind == kindData {
		if !info.IsDir() {
			return nil, protoErr("unsupported_source", false,
				"%s expects an IoTDB data directory; %s is a file — use %s for an archive of one",
				kindData, path, kindDataTar)
		}
		return inspectDir(path)
	}
	if info.IsDir() {
		return nil, protoErr("unsupported_source", false,
			"%s expects an archive file; %s is a directory — use %s for the directory itself",
			kindDataTar, path, kindData)
	}
	return inspectTar(path)
}

// inspectDir finds the data directory under the named path and hashes it.
func inspectDir(path string) (*source, *protoError) {
	root, perr := findDataRoot(path)
	if perr != nil {
		return nil, perr
	}
	sum, size, perr := treeChecksum(root)
	if perr != nil {
		return nil, perr
	}
	return &source{kind: kindData, path: root, checksum: sum, sizeBytes: size}, nil
}

// findDataRoot returns the one directory under path that holds both nodes'
// system properties.
func findDataRoot(path string) (string, *protoError) {
	var roots []string
	err := filepath.WalkDir(path, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if !d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(path, p)
		if rerr != nil {
			return rerr
		}
		if rel != "." && strings.Count(filepath.ToSlash(rel), "/")+1 > maxDataRootDepth {
			return fs.SkipDir
		}
		if isRegular(filepath.Join(p, confignodeProperties)) && isRegular(filepath.Join(p, datanodeProperties)) {
			roots = append(roots, p)
			return fs.SkipDir
		}
		return nil
	})
	if err != nil {
		return "", protoErr("source_unreadable", false, "walk backup directory: %v", err)
	}
	switch len(roots) {
	case 0:
		return "", protoErr("unsupported_source", false,
			"%s holds no IoTDB data directory: nothing within %d levels carries both %s and %s — the "+
				"artifact is the data directory of a stopped standalone node, or the target of "+
				"tools/ops/backup.sh", path, maxDataRootDepth, confignodeProperties, datanodeProperties)
	case 1:
		return roots[0], nil
	default:
		return "", protoErr("unsupported_source", false,
			"%s holds %d IoTDB data directories (%s and %s among them), and a drill restores one node: "+
				"name the one to restore", path, len(roots), roots[0], roots[1])
	}
}

// isRegular reports whether path is a regular file.
func isRegular(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// inspectTar judges an archive by its bytes: gzip or not, and non-empty.
// What is inside it is tar's verdict and then the engine's, after
// extraction.
func inspectTar(path string) (*source, *protoError) {
	sum, size, gz, perr := fileChecksum(path)
	if perr != nil {
		return nil, perr
	}
	if size == 0 {
		return nil, protoErr("source_corrupt", false, "%s is empty", filepath.Base(path))
	}
	return &source{kind: kindDataTar, path: path, checksum: sum, sizeBytes: size, gzip: gz}, nil
}

// treeChecksum hashes a directory as one artifact: every regular file's
// path relative to the directory and its bytes, in sorted path order, so
// the same tree always hashes the same way and a moved file changes the
// sum.
func treeChecksum(root string) (string, int64, *protoError) {
	h := sha256.New()
	var total int64
	var paths []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.Type().IsRegular() {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return "", 0, protoErr("source_unreadable", false, "walk backup directory: %v", err)
	}
	sort.Strings(paths)
	for _, path := range paths {
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return "", 0, protoErr("internal", false, "relative path of %s: %v", path, rerr)
		}
		if _, err := fmt.Fprintf(h, "%s\n", filepath.ToSlash(rel)); err != nil {
			return "", 0, protoErr("internal", false, "hash: %v", err)
		}
		n, perr := hashFile(h, path)
		if perr != nil {
			return "", 0, perr
		}
		total += n
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), total, nil
}

// fileChecksum hashes one file and reports whether its first bytes are the
// gzip magic. The bytes decide, never the name.
func fileChecksum(path string) (string, int64, bool, *protoError) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return "", 0, false, protoErr("source_unreadable", false, "open backup source: %v", err)
	}
	defer f.Close() //nolint:errcheck // read-only descriptor
	var magic [2]byte
	n, rerr := io.ReadFull(f, magic[:])
	if rerr != nil && !errors.Is(rerr, io.EOF) && !errors.Is(rerr, io.ErrUnexpectedEOF) {
		return "", 0, false, protoErr("source_unreadable", false, "read backup source: %v", rerr)
	}
	head := magic[:n]
	gz := len(head) == 2 && head[0] == 0x1f && head[1] == 0x8b
	h := sha256.New()
	h.Write(head)
	written, cerr := io.Copy(h, f)
	if cerr != nil {
		return "", 0, false, protoErr("source_unreadable", false, "read backup source: %v", cerr)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), written + int64(n), gz, nil
}

// hashFile streams one file into the running hash.
func hashFile(h io.Writer, path string) (int64, *protoError) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return 0, protoErr("source_unreadable", false, "open %s: %v", filepath.Base(path), err)
	}
	defer f.Close() //nolint:errcheck // read-only descriptor
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, protoErr("source_unreadable", false, "read %s: %v", filepath.Base(path), err)
	}
	return n, nil
}
