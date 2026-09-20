package main

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// transferArtifact moves the artifact into the sandbox: an archive is
// placed and unpacked, a directory is recreated file by file. It returns
// the directory arangorestore is pointed at.
func transferArtifact(ctx context.Context, c *core, src *resolvedSource,
	workDir string) (transferSeconds, unpackSeconds float64, dumpDir string, perr *protoError) {
	if src.tarball {
		return unpackArchive(ctx, c, src.path, workDir)
	}
	dumpDir = path.Join(workDir, "dump")
	transferSeconds, perr = transferDir(ctx, c, src.path, dumpDir)
	return transferSeconds, 0, dumpDir, perr
}

// rootScript locates the dump after unpacking: an operator's `tar -cf`
// either packs the dump's files at the archive root or one wrapping
// directory above them, so the script answers with whichever directory
// holds dump.json and descends exactly one level to find it.
const rootScript = `d="$1"
if [ -e "$d/` + manifestName + `" ]; then printf '%s\n' "$d"; exit 0; fi
for sub in "$d"/*/; do
  if [ -e "${sub}` + manifestName + `" ]; then printf '%s\n' "${sub%/}"; exit 0; fi
done
printf '%s\n' "$d"`

func unpackArchive(ctx context.Context, c *core, hostPath, workDir string) (transferSeconds, unpackSeconds float64, dumpDir string, perr *protoError) {
	extractDir := path.Join(workDir, "extract")
	if perr := mkdirAll(ctx, c, extractDir); perr != nil {
		return 0, 0, "", perr
	}
	tarPath := path.Join(workDir, tarName)
	put, perr := c.putFile(ctx, putFileArgs{SourcePath: hostPath, DestPath: tarPath, Mode: "0600"})
	if perr != nil {
		return 0, 0, "", perr
	}
	unpack, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"tar", "-xf", tarPath, "-C", extractDir}})
	if perr != nil {
		return 0, 0, "", perr
	}
	if unpack.ExitCode != 0 {
		return 0, 0, "", protoErr("source_corrupt", false,
			"tar could not unpack the archive: %s", firstLine(stderr))
	}
	locate, stdout, _, perr := c.exec(ctx, execArgs{Argv: []string{"sh", "-c", rootScript, "sh", extractDir}})
	if perr != nil {
		return 0, 0, "", perr
	}
	dumpDir = strings.TrimSpace(firstLine(stdout))
	if locate.ExitCode != 0 || dumpDir == "" {
		dumpDir = extractDir
	}
	return put.DurationSeconds, unpack.DurationSeconds + locate.DurationSeconds, dumpDir, nil
}

// dumpFile is one file the host will put into the sandbox.
type dumpFile struct{ host, dest string }

// transferDir recreates the dump inside the sandbox: one mkdir, then one
// put_file per file.
func transferDir(ctx context.Context, c *core, hostDir, dumpDir string) (float64, *protoError) {
	dirs, files, perr := walkDump(hostDir, dumpDir)
	if perr != nil {
		return 0, perr
	}
	if perr := mkdirAll(ctx, c, dirs...); perr != nil {
		return 0, perr
	}
	total := 0.0
	for _, f := range files {
		put, perr := c.putFile(ctx, putFileArgs{SourcePath: f.host, DestPath: f.dest, Mode: "0600"})
		if perr != nil {
			return 0, perr
		}
		total += put.DurationSeconds
	}
	return total, nil
}

// walkDump lists the directories and files of a dump.
func walkDump(hostDir, dumpDir string) ([]string, []dumpFile, *protoError) {
	dirs := []string{dumpDir}
	var files []dumpFile
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
			dirs = append(dirs, dumpDir+"/"+slash)
		} else if d.Type().IsRegular() {
			files = append(files, dumpFile{host: p, dest: dumpDir + "/" + slash})
		}
		return nil
	})
	if err != nil {
		return nil, nil, protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	return dirs, files, nil
}

func mkdirAll(ctx context.Context, c *core, dirs ...string) *protoError {
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: append([]string{"mkdir", "-p"}, dirs...)})
	if perr != nil {
		return perr
	}
	if val.ExitCode != 0 {
		return protoErr("internal", false, "prepare work directory: %s", firstLine(stderr))
	}
	return nil
}
