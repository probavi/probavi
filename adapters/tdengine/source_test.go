package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tarEntry is one member of a hand-built archive.
type tarEntry struct {
	name string
	body string
	// typeflag is a regular file when zero.
	typeflag byte
	// link is a symbolic link's target.
	link string
}

// writeTar builds an archive member by member, so a test says exactly what
// an archive holds and in which order the host meets it.
func writeTar(t *testing.T, entries ...tarEntry) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o600, Typeflag: e.typeflag, Linkname: e.link}
		if e.typeflag == 0 {
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write %s: %v", e.name, err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatalf("write %s: %v", e.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close archive: %v", err)
	}
	path := filepath.Join(t.TempDir(), "dump.tar")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// archiveDump archives a dump directory the way tar does, in lexical order
// and with its directories as members, every name under prefix: "" and
// "./" are `tar -C dump -cf x.tar .` without and with the dot, "dump/" is
// an archive of the directory itself.
func archiveDump(t *testing.T, root, prefix string) string {
	t.Helper()
	var entries []tarEntry
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." {
			return err
		}
		name := prefix + filepath.ToSlash(rel)
		if d.IsDir() {
			entries = append(entries, tarEntry{name: name + "/", typeflag: tar.TypeDir})
			return nil
		}
		body, err := os.ReadFile(path)
		entries = append(entries, tarEntry{name: name, body: string(body)})
		return err
	})
	if err != nil {
		t.Fatalf("archive %s: %v", root, err)
	}
	return writeTar(t, entries...)
}

// schemaFor is the dbs.sql taosdump writes into a dump's payload directory.
func schemaFor(database string) string {
	return "#!server_ver: ver:3.3.6.13\nCREATE DATABASE IF NOT EXISTS " + database +
		" REPLICA 1   DURATION 10d KEEP 3650d,3650d,3650d     PRECISION 'ms' ;\n"
}

// resultFor is the dump_result.txt taosdump writes beside it.
func resultFor(started, rows string) string {
	return "========== DUMP OUT ========== \n# DumpOut start time: " + started + "\n# total row count:          " + rows + "\n"
}

// claims is what an artifact says about itself, whichever kind carries it.
func claims(src *resolvedSource) string {
	createdAt := "<nil>"
	if src.createdAt != nil {
		createdAt = *src.createdAt
	}
	return fmt.Sprintf("database=%s keep=%d created_at=%s started=%s rows=%d rows_known=%v",
		src.database, src.keepDays, createdAt, src.started, src.rows, src.rowsKnown)
}

// fileSum is the identity an archive is recorded under: its bytes on disk.
func fileSum(t *testing.T, path string) (string, int64) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), int64(len(raw))
}

// TestAnArchiveSaysWhatTheDirectoryItHoldsSays pins the promise resolveTar
// is written for: read host-side in one pass, an archive tells the fences
// everything its directory would — so the retention fence and the row
// count verdict hold for all three kinds, not two.
func TestAnArchiveSaysWhatTheDirectoryItHoldsSays(t *testing.T) {
	for name, tc := range map[string]struct {
		opts   dumpOptions
		prefix string
	}{
		"the output directory, members at the top": {
			dumpOptions{database: "plant", nested: true, keepDays: 30, startedAgo: 2 * time.Hour, rows: 480}, "",
		},
		"the output directory, members under ./": {
			dumpOptions{database: "plant", nested: true, keepDays: 30, startedAgo: 2 * time.Hour, rows: 480}, "./",
		},
		"the output directory archived as itself": {
			dumpOptions{database: "plant", nested: true, keepDays: 30, startedAgo: 2 * time.Hour, rows: 480}, "dump/",
		},
		"the payload directory alone": {
			dumpOptions{database: "plant", startedAgo: 2 * time.Hour}, "",
		},
		"a dump that recorded nothing about itself": {
			dumpOptions{nested: true, rows: -1}, "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			dump := writeDump(t, tc.opts)
			dir, perr := resolveSource(context.Background(), "taosdump", dump)
			if perr != nil {
				t.Fatalf("the directory: %+v", perr)
			}
			archive := archiveDump(t, dump, tc.prefix)
			src, perr := resolveSource(context.Background(), "taosdump_tar", archive)
			if perr != nil {
				t.Fatalf("the archive: %+v", perr)
			}
			if got, want := claims(src), claims(dir); got != want {
				t.Errorf("the archive claims %s\n  its directory claims %s", got, want)
			}
			sum, size := fileSum(t, archive)
			if src.checksum != sum || src.sizeBytes != size {
				t.Errorf("identity = %s / %d, want the archive's bytes %s / %d", src.checksum, src.sizeBytes, sum, size)
			}
			if !src.tarball || src.path != archive {
				t.Errorf("tarball = %v, path = %s: an archive moves as the file the drill named", src.tarball, src.path)
			}
		})
	}
}

// gzipBytes compresses content the way `tar -czf` does.
func gzipBytes(t *testing.T, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestTheBytesDecideWhetherAnArchiveIsCompressed pins the reading of a
// gzip archive: it claims what the same archive uncompressed claims, is
// identified by the bytes the drill named, and neither name misleads the
// host — a gzip archive called dump.tar is read through gzip, and a plain
// one called dump.tar.gz is not.
func TestTheBytesDecideWhetherAnArchiveIsCompressed(t *testing.T) {
	plain := archiveDump(t, writeDump(t, dumpOptions{database: "plant", nested: true, keepDays: 30, startedAgo: time.Hour}), "")
	raw, err := os.ReadFile(plain)
	if err != nil {
		t.Fatal(err)
	}
	misnamed := filepath.Join(t.TempDir(), "dump.tar.gz")
	compressed := filepath.Join(t.TempDir(), "dump.tar")
	for path, content := range map[string][]byte{misnamed: raw, compressed: gzipBytes(t, raw)} {
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, perr := resolveSource(context.Background(), "taosdump_tar", plain)
	if perr != nil {
		t.Fatalf("the plain archive: %+v", perr)
	}
	for _, archive := range []string{misnamed, compressed} {
		src, perr := resolveSource(context.Background(), "taosdump_tar", archive)
		if perr != nil {
			t.Fatalf("%s: %+v", archive, perr)
		}
		if got := claims(src); got != claims(want) {
			t.Errorf("%s claims %s\n  the plain archive claims %s", archive, got, claims(want))
		}
		if sum, size := fileSum(t, archive); src.checksum != sum || src.sizeBytes != size {
			t.Errorf("%s identity = %s / %d, want its bytes on disk %s / %d", archive, src.checksum, src.sizeBytes, sum, size)
		}
	}
}

// TestTheFirstOfEachFileInAnArchiveIsTheOneRead pins takeMeta's rule: the
// first schema that creates a database and the first result file win, and
// only regular members are read at all — so a nested archive cannot talk
// over the dump that holds it, and a link cannot point the host elsewhere.
func TestTheFirstOfEachFileInAnArchiveIsTheOneRead(t *testing.T) {
	archive := writeTar(t,
		tarEntry{name: "dump/", typeflag: tar.TypeDir},
		tarEntry{name: "dump/links/dbs.sql", typeflag: tar.TypeSymlink, link: "/etc/passwd"},
		tarEntry{name: "dump/dirs/dbs.sql/", typeflag: tar.TypeDir},
		tarEntry{name: "dump/dbs.sql", body: "#!server_ver: ver:3.3.6.13\n#!dumpdb: first: \n"},
		tarEntry{name: "dump/dump_result.txt", body: resultFor("2026-09-10 08:00:00", "250")},
		tarEntry{name: "dump/taosdump.1/dbs.sql", body: schemaFor("first")},
		tarEntry{name: "dump/taosdump.1/old/dump_result.txt", body: resultFor("2020-01-01 00:00:00", "999")},
		tarEntry{name: "dump/taosdump.1/old/dbs.sql", body: schemaFor("second")},
	)
	src, perr := resolveSource(context.Background(), "taosdump_tar", archive)
	if perr != nil {
		t.Fatalf("resolve: %+v", perr)
	}
	if src.database != "first" || src.keepDays != 3650 {
		t.Errorf("database = %q keep = %d, want the first schema that creates one", src.database, src.keepDays)
	}
	if src.rows != 250 || src.createdAt == nil || *src.createdAt != "2026-09-10T08:00:00Z" {
		t.Errorf("rows = %d created_at = %v, want the first result file", src.rows, src.createdAt)
	}
}

// TestAnOversizedMemberIsReadOnlyToItsCap: the host reads an archive before
// the drill has decided anything, so a member named like metadata gives up
// no more than metadata could be — and the walk goes on past it to the
// members after.
func TestAnOversizedMemberIsReadOnlyToItsCap(t *testing.T) {
	padding := strings.Repeat("-- a schema is kilobytes, and this is not one\n", maxMetaBytes/40)
	archive := writeTar(t,
		tarEntry{name: "taosdump.1/dbs.sql", body: schemaFor("big") + padding},
		tarEntry{name: "dump_result.txt", body: resultFor("2026-09-10 08:00:00", "42")},
	)
	schema, result, perr := inspectDumpTar(archive)
	if perr != nil {
		t.Fatalf("inspect: %+v", perr)
	}
	if len(schema) != maxMetaBytes {
		t.Errorf("read %d bytes of the schema, want the cap of %d", len(schema), maxMetaBytes)
	}
	if rows, known := artifactRows(result); !known || rows != 42 {
		t.Errorf("rows = %d (known %v), want the result file after the oversized member", rows, known)
	}
	src, perr := resolveTar(archive)
	if perr != nil || src.database != "big" {
		t.Errorf("resolve = %+v, %+v, want the database the capped schema still names", src, perr)
	}
}

// TestAnArchiveIsRefusedForWhatItIsNot covers every refusal an archive can
// earn on the host, each in the words and the code that say whose problem
// it is.
func TestAnArchiveIsRefusedForWhatItIsNot(t *testing.T) {
	valid := writeTar(t, tarEntry{name: "taosdump.1/dbs.sql", body: schemaFor("drill")})
	raw, err := os.ReadFile(valid)
	if err != nil {
		t.Fatal(err)
	}
	gzipped := gzipBytes(t, bytes.Repeat(raw, 8))
	write := func(name string, content []byte) string {
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	for name, tc := range map[string]struct {
		path, code, message string
	}{
		"a path that does not exist": {
			filepath.Join(t.TempDir(), "gone.tar"), "source_not_found", "does not exist",
		},
		"a path beneath a file": {
			filepath.Join(valid, "dump.tar"), "source_unreadable", "stat backup source",
		},
		"a directory": {
			writeDump(t, dumpOptions{nested: true}), "invalid_request", "use kind taosdump",
		},
		"an empty file": {
			write("empty.tar", nil), "source_corrupt", "holds no taosdump backup",
		},
		"bytes that are not an archive": {
			write("text.tar", bytes.Repeat([]byte("not a tar archive\n"), 64)), "source_corrupt", "read archive",
		},
		"an archive cut off inside a member header": {
			write("cut.tar", raw[:300]), "source_corrupt", "read archive: unexpected EOF",
		},
		"an archive cut off inside the schema": {
			write("cut.tar", raw[:600]), "source_corrupt", "read dbs.sql from the archive",
		},
		"an archive of the directory beside a dump": {
			archiveDump(t, writeDump(t, dumpOptions{noSchema: true}), ""), "source_corrupt", "holds no taosdump backup",
		},
		"gzip bytes that do not decompress": {
			write("dump.tar", append([]byte{0x1f, 0x8b}, bytes.Repeat([]byte("x"), 64)...)), "source_corrupt", "read archive: gzip",
		},
		"a gzip archive cut off": {
			write("dump.tar", gzipped[:len(gzipped)/2]), "source_corrupt", "unexpected EOF",
		},
		// An intact backup in a compression this adapter does not read is
		// not a damaged one, and the code says whose problem it is.
		"a bzip2 archive": {
			write("dump.tar", []byte("BZh91AY&SY")), "unsupported_source", "bzip2-compressed",
		},
		"an xz archive": {
			write("dump.tar", []byte{0xfd, '7', 'z', 'X', 'Z', 0x00, 0x00, 0x04}), "unsupported_source", "xz-compressed",
		},
		"a zstd archive": {
			write("dump.tar", []byte{0x28, 0xb5, 0x2f, 0xfd, 0x00, 0x58}), "unsupported_source", "zstd-compressed",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, perr := resolveSource(context.Background(), "taosdump_tar", tc.path)
			if perr == nil || perr.Code != tc.code || !strings.Contains(perr.Message, tc.message) {
				t.Errorf("got %+v, want %s mentioning %q", perr, tc.code, tc.message)
			}
		})
	}
}

func TestAnArchiveTheHostCannotOpenIsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 file")
	}
	archive := writeTar(t, tarEntry{name: "taosdump.1/dbs.sql", body: schemaFor("drill")})
	if err := os.Chmod(archive, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(archive, 0o600); err != nil {
			t.Errorf("restore the mode: %v", err)
		}
	})
	if _, perr := resolveTar(archive); perr == nil || perr.Code != "source_unreadable" || !strings.Contains(perr.Message, "open archive") {
		t.Errorf("got %+v, want source_unreadable", perr)
	}
}

// placeDump moves a fixture dump into a directory of dumps under a name,
// dated on disk as mtime.
func placeDump(t *testing.T, parent, name string, opts dumpOptions, mtime time.Time) string {
	t.Helper()
	path := filepath.Join(parent, name)
	if err := os.Rename(writeDump(t, opts), path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestTheNewestDumpIsTheOneThatRecordsItself pins taosdump_dir's choice: a
// dump is dated by the instant it records, because a file's time dates the
// copy — and only a dump that records none falls back to its directory's.
func TestTheNewestDumpIsTheOneThatRecordsItself(t *testing.T) {
	now := time.Now()
	parent := t.TempDir()
	// Copied last, taken yesterday.
	placeDump(t, parent, "a", dumpOptions{database: "yesterday", nested: true, startedAgo: 24 * time.Hour}, now)
	want := placeDump(t, parent, "b", dumpOptions{database: "lasthour", nested: true, startedAgo: time.Hour}, now.Add(-48*time.Hour))
	placeDump(t, parent, "c", dumpOptions{database: "undated", nested: true}, now.Add(-72*time.Hour))
	// Two dumps under one output directory are skipped rather than
	// guessed between, however new they claim to be.
	two := placeDump(t, parent, "d", dumpOptions{database: "ambiguous", nested: true, startedAgo: time.Minute}, now)
	second := filepath.Join(two, "taosdump.2", "dbs.sql")
	if err := os.MkdirAll(filepath.Dir(second), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte(schemaFor("other")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(parent, "e-not-a-dump"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "f-notes.txt"), []byte("not a dump"), 0o600); err != nil {
		t.Fatal(err)
	}

	src, perr := resolveSource(context.Background(), "taosdump_dir", parent)
	if perr != nil {
		t.Fatalf("resolve: %+v", perr)
	}
	if src.database != "lasthour" || !strings.HasPrefix(src.path, want+string(filepath.Separator)) {
		t.Errorf("chose %s (%s), want the dump that records the latest start under %s", src.database, src.path, want)
	}
}

func TestADumpThatRecordsNoInstantIsDatedByItsDirectory(t *testing.T) {
	now := time.Now()
	parent := t.TempDir()
	placeDump(t, parent, "a", dumpOptions{database: "yesterday", nested: true, startedAgo: 24 * time.Hour}, now)
	placeDump(t, parent, "b", dumpOptions{database: "undated", nested: true}, now.Add(-time.Hour))
	src, perr := resolveSource(context.Background(), "taosdump_dir", parent)
	if perr != nil {
		t.Fatalf("resolve: %+v", perr)
	}
	if src.database != "undated" {
		t.Errorf("chose %s, want the undated dump whose directory is newer than the other's record", src.database)
	}
}

func TestADirectoryOfDumpsIsRefusedForWhatItIsNot(t *testing.T) {
	file := filepath.Join(t.TempDir(), "dumps")
	if err := os.WriteFile(file, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for name, tc := range map[string]struct {
		ctx                 context.Context
		path, code, message string
	}{
		"a path that does not exist": {
			context.Background(), filepath.Join(t.TempDir(), "gone"), "source_not_found", "does not exist",
		},
		"a file": {context.Background(), file, "source_unreadable", "read backup directory"},
		"a directory holding no dump": {
			context.Background(), filepath.Dir(file), "source_not_found", "holds no taosdump backup",
		},
		"a drill cancelled while choosing": {cancelled, filepath.Dir(file), "cancelled", "choosing a backup"},
	} {
		t.Run(name, func(t *testing.T) {
			_, perr := resolveSource(tc.ctx, "taosdump_dir", tc.path)
			if perr == nil || perr.Code != tc.code || !strings.Contains(perr.Message, tc.message) {
				t.Errorf("got %+v, want %s mentioning %q", perr, tc.code, tc.message)
			}
		})
	}
}

// archiveHandler answers a provision of an archive the way a healthy
// sandbox does, handing each exec to answer first so a test can replace
// one step's answer.
func archiveHandler(t *testing.T, archive string, seen *[]string, answer func(step string, args execArgs) any) func(verbCall) (any, *protoError) {
	healthy := happyHandler(t, seen)
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			*seen = append(*seen, "put_file")
			args := putFileArgs{}
			if err := json.Unmarshal(call.Args, &args); err != nil {
				t.Fatalf("put_file args: %v", err)
			}
			if args.SourcePath != archive || args.DestPath != archivePath {
				t.Errorf("put_file %s -> %s, want the archive itself at %s", args.SourcePath, args.DestPath, archivePath)
			}
			return putFileValue{BytesCopied: 4096, DurationSeconds: 0.4}, nil
		}
		if v := answer(step(t, call), parseExec(t, call)); v != nil {
			*seen = append(*seen, step(t, call))
			return v, nil
		}
		return healthy(call)
	}
}

// settledArchive is an archive of a whole dump, dated an hour ago on disk
// so the settle check has nothing to wait for.
func settledArchive(t *testing.T, opts dumpOptions) string {
	t.Helper()
	archive := archiveDump(t, writeDump(t, opts), "")
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(archive, old, old); err != nil {
		t.Fatal(err)
	}
	return archive
}

// TestAnArchiveIsUnpackedInTheSandbox pins the archive's path through a
// drill: the file moves as it is, the sandbox unpacks it and names the
// directory the schema is in, the restore tool is pointed there, and the
// unpacking is transfer time rather than restore time.
func TestAnArchiveIsUnpackedInTheSandbox(t *testing.T) {
	archive := settledArchive(t, dumpOptions{database: "plant", nested: true, startedAgo: time.Hour})
	unpacked := stagingDir + "/taosdump.3578289329408"
	var seen []string
	var restoredFrom string
	line, _, exit := driveOp(t, "provision", provisionPayload(archive, "taosdump_tar"),
		archiveHandler(t, archive, &seen, func(step string, args execArgs) any {
			switch step {
			case "extract":
				if len(args.Argv) != 6 || args.Argv[4] != stagingDir || args.Argv[5] != archivePath {
					t.Errorf("extract argv = %q, want the staging directory and the archive", args.Argv)
				}
				return execValue{StdoutB64: base64.StdEncoding.EncodeToString([]byte(unpacked + "\n")), DurationSeconds: 1.5}
			case "restore":
				restoredFrom = args.Argv[len(args.Argv)-1]
			}
			return nil
		}))
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	f := parseFinal(t, line)
	if !f.OK {
		t.Fatalf("final = %+v", f)
	}
	if want := "ready,put_file,extract,restore,tables"; strings.Join(seen, ",") != want {
		t.Errorf("steps = %v, want %s", seen, want)
	}
	if restoredFrom != unpacked {
		t.Errorf("restored from %q, want the directory the sandbox found the schema in", restoredFrom)
	}
	payload := struct {
		Connection struct {
			Database string `json:"database"`
		} `json:"connection"`
		SourceIdentity struct {
			Checksum string `json:"checksum"`
		} `json:"source_identity"`
		Timings struct {
			Transfer float64 `json:"transfer_seconds"`
		} `json:"timings"`
	}{}
	if err := json.Unmarshal(f.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if sum, _ := fileSum(t, archive); payload.SourceIdentity.Checksum != sum {
		t.Errorf("checksum = %s, want the archive's %s", payload.SourceIdentity.Checksum, sum)
	}
	if payload.Connection.Database != "plant" {
		t.Errorf("database = %q, want the one the archive's schema creates", payload.Connection.Database)
	}
	if payload.Timings.Transfer != 1.9 {
		t.Errorf("transfer_seconds = %v, want the copy and the unpacking together (1.9)", payload.Timings.Transfer)
	}
}

func TestAnArchiveTheSandboxCannotUseIsCorrupt(t *testing.T) {
	for name, tc := range map[string]struct {
		extract execValue
		message string
	}{
		"tar refuses it": {
			execValue{ExitCode: 90, StderrB64: base64.StdEncoding.EncodeToString([]byte(
				"\ntar: Unexpected EOF in archive\ntar: Error is not recoverable: exiting now\n"))},
			"could not be unpacked: tar: Unexpected EOF in archive",
		},
		"nothing unpacked carries a schema": {
			execValue{}, "nothing inside it carries a dbs.sql",
		},
		"the search for the schema fails": {
			execValue{ExitCode: 1, StdoutB64: base64.StdEncoding.EncodeToString([]byte(stagingDir + "/x\n"))},
			"nothing inside it carries a dbs.sql",
		},
	} {
		t.Run(name, func(t *testing.T) {
			archive := settledArchive(t, dumpOptions{nested: true, startedAgo: time.Hour})
			var seen []string
			line, _, exit := driveOp(t, "provision", provisionPayload(archive, "taosdump_tar"),
				archiveHandler(t, archive, &seen, func(step string, _ execArgs) any {
					if step == "extract" {
						return tc.extract
					}
					if step == "restore" {
						t.Error("the restore ran from an archive that did not unpack")
					}
					return nil
				}))
			if exit != 0 {
				t.Fatalf("exit = %d", exit)
			}
			f := parseFinal(t, line)
			if f.OK || f.Error == nil || f.Error.Code != "source_corrupt" || !strings.Contains(f.Error.Message, tc.message) {
				t.Errorf("final = %+v, want source_corrupt mentioning %q", f, tc.message)
			}
		})
	}
}

// TestAnArchiveOlderThanItsRetentionIsRefusedBeforeTheSandbox: the fence
// reads what the host read out of the archive, so it costs an archive no
// more than it costs a directory.
func TestAnArchiveOlderThanItsRetentionIsRefusedBeforeTheSandbox(t *testing.T) {
	archive := settledArchive(t, dumpOptions{nested: true, keepDays: 3, startedAgo: 10 * 24 * time.Hour})
	line, calls, exit := driveOp(t, "provision", provisionPayload(archive, "taosdump_tar"),
		func(verbCall) (any, *protoError) {
			t.Error("the sandbox was touched for a drill that cannot run")
			return okExec(0), nil
		})
	if exit != 0 || len(calls) != 0 {
		t.Fatalf("exit = %d, sandbox calls = %d", exit, len(calls))
	}
	f := parseFinal(t, line)
	if f.OK || f.Error == nil || f.Error.Code != "restore_failed" || !strings.Contains(f.Error.Message, "KEEP 3d") {
		t.Errorf("final = %+v, want restore_failed naming KEEP 3d", f)
	}
}
