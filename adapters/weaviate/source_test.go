package main

import (
	"archive/tar"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureCompletedAt is the completion instant every fixture backup
// claims unless a spec overrides it — the shape the engine writes:
// RFC 3339 UTC, sub-second precision, zone attached (measured).
const fixtureCompletedAt = "2026-09-03T15:58:20.0845266Z"

// backupSpec describes a fixture backup tree; zero values mean the
// healthy single-node, single-class default.
type backupSpec struct {
	id          string // defaults to the directory name
	node        string // defaults to node1
	status      string // defaults to SUCCESS
	startedAt   string
	completedAt string
	classes     []string // defaults to [Books]
	metaError   string
	secondNode  bool // a two-node backup
	dropChunk   bool // the node manifest names a chunk the tree lacks
	dropNode    bool // no <node>/backup.json
	corruptMeta bool // backup_config.json is not JSON
	noMeta      bool // no backup_config.json at all
}

// writeBackupFixture builds one backup directory in the shape the engine
// writes (measured): backup_config.json at the root, <node>/backup.json,
// and <node>/<Class>/chunk-1 per class.
func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func (spec backupSpec) withDefaults(name string) backupSpec {
	spec.id = orDefault(spec.id, name)
	spec.node = orDefault(spec.node, "node1")
	spec.status = orDefault(spec.status, "SUCCESS")
	spec.startedAt = orDefault(spec.startedAt, "2026-09-03T15:58:18.0819484Z")
	spec.completedAt = orDefault(spec.completedAt, fixtureCompletedAt)
	if spec.classes == nil {
		spec.classes = []string{"Books"}
	}
	return spec
}

func writeBackupFixture(t *testing.T, parent, name string, spec backupSpec) string {
	t.Helper()
	spec = spec.withDefaults(name)
	dir := filepath.Join(parent, name)

	nodes := map[string]any{spec.node: map[string]any{"classes": spec.classes, "status": spec.status}}
	if spec.secondNode {
		nodes["node2"] = map[string]any{"classes": spec.classes, "status": spec.status}
	}
	meta := map[string]any{
		"startedAt": spec.startedAt, "completedAt": spec.completedAt,
		"id": spec.id, "nodes": nodes, "status": spec.status, "error": spec.metaError,
		"serverVersion": "1.39.2", "version": "2.1", "compressionType": "gzip",
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	if spec.corruptMeta {
		metaJSON = metaJSON[:len(metaJSON)/2]
	}

	classes := make([]map[string]any, 0, len(spec.classes))
	for _, c := range spec.classes {
		classes = append(classes, map[string]any{
			"name": c, "backupId": spec.id,
			"chunks": map[string]any{"1": []string{"shardA"}},
		})
	}
	nodeJSON, err := json.Marshal(map[string]any{
		"id": spec.id, "classes": classes, "status": spec.status,
	})
	if err != nil {
		t.Fatalf("marshal node meta: %v", err)
	}

	writeFixtureFile := func(rel string, body []byte) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(p, body, 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	if !spec.noMeta {
		writeFixtureFile(metaFileName, metaJSON)
	}
	if !spec.dropNode {
		writeFixtureFile(filepath.Join(spec.node, nodeMetaFileName), nodeJSON)
	}
	for _, c := range spec.classes {
		if spec.dropChunk {
			continue
		}
		writeFixtureFile(filepath.Join(spec.node, c, "chunk-1"), []byte("gzip-compressed chunk bytes"))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	return dir
}

// tarOf packs a fixture directory the way an operator would: the backup
// directory itself as the one wrapping entry.
func tarOf(t *testing.T, dir string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), filepath.Base(dir)+".tar")
	f, err := os.Create(out)
	if err != nil {
		t.Fatalf("create tar: %v", err)
	}
	tw := tar.NewWriter(f)
	root := filepath.Dir(dir)
	err = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if d.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		_, err = tw.Write(body)
		return err
	})
	if err != nil {
		t.Fatalf("pack %s: %v", dir, err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	return out
}

func TestResolveSourceAcceptsEveryKind(t *testing.T) {
	parent := t.TempDir()
	dir := writeBackupFixture(t, parent, "nightly", backupSpec{})
	archive := tarOf(t, dir)

	multi := t.TempDir()
	writeBackupFixture(t, multi, "older", backupSpec{completedAt: "2026-09-01T00:00:00Z"})
	writeBackupFixture(t, multi, "newer", backupSpec{completedAt: "2026-09-02T00:00:00Z"})

	for _, tc := range []struct {
		kind, path, wantSuffix string
		wantMeta               bool
	}{
		{kind: "weaviate_backup_tar", path: archive, wantSuffix: "nightly.tar"},
		{kind: "weaviate_backup", path: dir, wantSuffix: "nightly", wantMeta: true},
		{kind: "weaviate_backup_dir", path: multi, wantSuffix: "newer", wantMeta: true},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			src, perr := resolveSource(tc.kind, tc.path, nil)
			if perr != nil {
				t.Fatalf("resolve: %+v", perr)
			}
			if !strings.HasSuffix(src.path, tc.wantSuffix) {
				t.Errorf("resolved %q, want suffix %q", src.path, tc.wantSuffix)
			}
			if !strings.HasPrefix(src.checksum, "sha256:") || src.sizeBytes == 0 {
				t.Errorf("identity = %q / %d bytes", src.checksum, src.sizeBytes)
			}
			assertMetaExpectation(t, src, tc.wantMeta)
		})
	}
}

func assertMetaExpectation(t *testing.T, src *resolvedSource, wantMeta bool) {
	t.Helper()
	if !wantMeta {
		if src.meta != nil {
			t.Error("the archive kind must not read content on the host")
		}
		return
	}
	if src.meta == nil || src.node != "node1" || len(src.classes) != 1 {
		t.Errorf("meta not read: node %q classes %v", src.node, src.classes)
	}
	if src.createdAt == nil {
		t.Error("created_at not taken from the backup's own completion instant")
	}
}

// TestTheArchiveKindReadsNoContent: a file of random bytes resolves — the
// sandbox's tar and then the engine judge it, which is what lets the
// conformance suite's generated source reach the sandbox.
func TestTheArchiveKindReadsNoContent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "garbage.tar")
	if err := os.WriteFile(p, []byte("not a tar at all"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	src, perr := resolveSource("weaviate_backup_tar", p, nil)
	if perr != nil {
		t.Fatalf("an unreadable archive must pass the host: %+v", perr)
	}
	if !src.tarball {
		t.Error("archive not marked for in-sandbox unpacking")
	}
}

func TestBackupDirGates(t *testing.T) {
	for _, tc := range []struct {
		name     string
		spec     backupSpec
		wantCode string
		wantSay  string
	}{
		{name: "no metadata is not a backup", spec: backupSpec{noMeta: true},
			wantCode: "source_corrupt", wantSay: "POST /v1/backups/filesystem"},
		{name: "corrupt metadata", spec: backupSpec{corruptMeta: true},
			wantCode: "source_corrupt", wantSay: "not the JSON"},
		{name: "a failed backup never became an artifact",
			spec:     backupSpec{status: "FAILED", metaError: "node ran out of disk"},
			wantCode: "source_corrupt", wantSay: "fix the job"},
		{name: "an in-progress backup is not yet an artifact",
			spec:     backupSpec{status: "STARTED"},
			wantCode: "source_unreadable", wantSay: "still writing"},
		{name: "a multi-node backup cannot be proven here",
			spec:     backupSpec{secondNode: true},
			wantCode: "invalid_request", wantSay: "single-node"},
		{name: "a chunk the manifest names is missing",
			spec:     backupSpec{dropChunk: true},
			wantCode: "source_corrupt", wantSay: "chunk"},
		{name: "the node manifest is missing",
			spec:     backupSpec{dropNode: true},
			wantCode: "source_corrupt", wantSay: nodeMetaFileName},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeBackupFixture(t, t.TempDir(), "b", tc.spec)
			_, perr := resolveSource("weaviate_backup", dir, nil)
			if perr == nil {
				t.Fatal("expected a refusal")
			}
			if perr.Code != tc.wantCode {
				t.Errorf("code %q, want %q (%s)", perr.Code, tc.wantCode, perr.Message)
			}
			if !strings.Contains(perr.Message, tc.wantSay) {
				t.Errorf("message %q does not say %q", perr.Message, tc.wantSay)
			}
		})
	}
}

// TestChooseBackupInPrefersTheBackupsOwnClaim: ranking is by the
// completion instant each backup states about itself, never file times —
// and a newer attempt that is not a completed backup refuses the drill by
// name rather than being silently passed over.
func TestChooseBackupInRanks(t *testing.T) {
	t.Run("the claimed instant ranks, not file times", func(t *testing.T) {
		parent := t.TempDir()
		// Written second (newer mtime), completed earlier.
		writeBackupFixture(t, parent, "written-later", backupSpec{completedAt: "2026-09-01T00:00:00Z"})
		writeBackupFixture(t, parent, "completed-later", backupSpec{completedAt: "2026-09-02T00:00:00Z"})
		winner, perr := chooseBackupIn(parent, selectNewest)
		if perr != nil {
			t.Fatalf("rank: %+v", perr)
		}
		if filepath.Base(winner) != "completed-later" {
			t.Errorf("winner %q, want completed-later", winner)
		}
	})

	t.Run("ties break toward the larger name", func(t *testing.T) {
		parent := t.TempDir()
		writeBackupFixture(t, parent, "a", backupSpec{})
		writeBackupFixture(t, parent, "b", backupSpec{})
		winner, perr := chooseBackupIn(parent, selectNewest)
		if perr != nil {
			t.Fatalf("rank: %+v", perr)
		}
		if filepath.Base(winner) != "b" {
			t.Errorf("winner %q, want b", winner)
		}
	})

}

func TestChooseBackupInRefusesNewerAttempts(t *testing.T) {
	t.Run("a newer in-progress attempt refuses by name", func(t *testing.T) {
		parent := t.TempDir()
		writeBackupFixture(t, parent, "good", backupSpec{
			startedAt: "2026-09-01T00:00:00Z", completedAt: "2026-09-01T00:05:00Z"})
		writeBackupFixture(t, parent, "running", backupSpec{
			status: "TRANSFERRING", startedAt: "2026-09-02T00:00:00Z",
			completedAt: "0001-01-01T00:00:00Z"})
		_, perr := chooseBackupIn(parent, selectNewest)
		if perr == nil {
			t.Fatal("a directory with a newer in-progress attempt must refuse")
		}
		if perr.Code != "source_unreadable" || !strings.Contains(perr.Message, "running") {
			t.Errorf("refusal = %+v, want source_unreadable naming the running attempt", perr)
		}
	})

	t.Run("a newer failed attempt refuses by name", func(t *testing.T) {
		parent := t.TempDir()
		writeBackupFixture(t, parent, "good", backupSpec{
			startedAt: "2026-09-01T00:00:00Z", completedAt: "2026-09-01T00:05:00Z"})
		writeBackupFixture(t, parent, "broken", backupSpec{
			status: "FAILED", startedAt: "2026-09-02T00:00:00Z",
			completedAt: "0001-01-01T00:00:00Z"})
		_, perr := chooseBackupIn(parent, selectNewest)
		if perr == nil {
			t.Fatal("a directory whose newest attempt failed must refuse")
		}
		if perr.Code != "source_corrupt" || !strings.Contains(perr.Message, "broken") {
			t.Errorf("refusal = %+v, want source_corrupt naming the failed attempt", perr)
		}
	})

}

func TestChooseBackupInCensus(t *testing.T) {
	t.Run("nothing restorable reports the census", func(t *testing.T) {
		parent := t.TempDir()
		if err := os.WriteFile(filepath.Join(parent, "notes.txt"), []byte("x"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := os.Mkdir(filepath.Join(parent, "not-a-backup"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		_, perr := chooseBackupIn(parent, selectNewest)
		if perr == nil || perr.Code != "source_not_found" {
			t.Fatalf("refusal = %+v, want source_not_found", perr)
		}
		if !strings.Contains(perr.Message, "2 entries") {
			t.Errorf("message %q does not report the census", perr.Message)
		}
	})
}

func TestCreatedAtFrom(t *testing.T) {
	if got := createdAtFrom(&backupMeta{CompletedAt: fixtureCompletedAt}); got == nil || *got != fixtureCompletedAt {
		t.Errorf("createdAtFrom = %v, want the instant as stated", got)
	}
	if got := createdAtFrom(&backupMeta{CompletedAt: "yesterdayish"}); got != nil {
		t.Errorf("createdAtFrom(garbage) = %q, want nil — a wrong instant is worse than none", *got)
	}
}

// TestDirChecksumIsCanonical: the same tree always hashes the same, and
// any content change changes the hash — this is the backup identity the
// evidence record carries.
func TestDirChecksumIsCanonical(t *testing.T) {
	a := writeBackupFixture(t, t.TempDir(), "same", backupSpec{})
	b := writeBackupFixture(t, t.TempDir(), "same", backupSpec{})
	sumA, _, perr := dirChecksum(a)
	if perr != nil {
		t.Fatalf("checksum: %+v", perr)
	}
	sumB, _, perr := dirChecksum(b)
	if perr != nil {
		t.Fatalf("checksum: %+v", perr)
	}
	if sumA != sumB {
		t.Errorf("identical trees hash differently: %s vs %s", sumA, sumB)
	}
	if err := os.WriteFile(filepath.Join(b, "node1", "Books", "chunk-1"), []byte("changed"), 0o600); err != nil {
		t.Fatalf("change: %v", err)
	}
	sumC, _, perr := dirChecksum(b)
	if perr != nil {
		t.Fatalf("checksum: %+v", perr)
	}
	if sumC == sumA {
		t.Error("a changed chunk did not change the tree hash")
	}
}

// TestMetaReadIsBounded: an operator-controlled "manifest" larger than
// the cap is refused rather than read to its end.
func TestMetaReadIsBounded(t *testing.T) {
	parent := t.TempDir()
	dir := writeBackupFixture(t, parent, "big", backupSpec{})
	huge := bytesOfSize(maxMetaBytes + 1)
	if err := os.WriteFile(filepath.Join(dir, metaFileName), huge, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, perr := resolveSource("weaviate_backup", dir, nil)
	if perr == nil || perr.Code != "source_corrupt" {
		t.Fatalf("refusal = %+v, want source_corrupt for an implausible manifest", perr)
	}
	if !strings.Contains(perr.Message, "MiB") {
		t.Errorf("message %q does not name the bound", perr.Message)
	}
}

func bytesOfSize(n int) []byte {
	b := make([]byte, n)
	copy(b, "{")
	return b
}

func TestRejectBackupTimezone(t *testing.T) {
	if perr := rejectBackupTimezone(nil); perr != nil {
		t.Errorf("no params must pass: %+v", perr)
	}
	perr := rejectBackupTimezone(map[string]string{"backup_timezone": "UTC"})
	if perr == nil || perr.Code != "invalid_request" {
		t.Errorf("backup_timezone = %+v, want invalid_request", perr)
	}
}

func TestTreeEntries(t *testing.T) {
	dir := writeBackupFixture(t, t.TempDir(), "tree", backupSpec{})
	dirs, files, perr := treeEntries(dir, "/scratch/probavi-weaviate/backups/tree")
	if perr != nil {
		t.Fatalf("treeEntries: %+v", perr)
	}
	wantFiles := map[string]bool{
		"/scratch/probavi-weaviate/backups/tree/" + metaFileName:           true,
		"/scratch/probavi-weaviate/backups/tree/node1/" + nodeMetaFileName: true,
		"/scratch/probavi-weaviate/backups/tree/node1/Books/chunk-1":       true,
	}
	if len(files) != len(wantFiles) {
		t.Fatalf("files = %v, want %d entries", files, len(wantFiles))
	}
	for _, f := range files {
		if !wantFiles[f.dest] {
			t.Errorf("unexpected destination %q", f.dest)
		}
	}
	wantDirs := 3 // the root, node1, node1/Books
	if len(dirs) != wantDirs {
		t.Errorf("dirs = %v, want %d entries", dirs, wantDirs)
	}
	for _, d := range dirs {
		if strings.Contains(d, "\\") {
			t.Errorf("destination %q is not in sandbox (slash) form", d)
		}
	}
}

func TestSingleNodeNamesTheNode(t *testing.T) {
	meta := &backupMeta{Nodes: map[string]metaNode{"prod-7": {}}}
	node, perr := singleNode(meta, "b")
	if perr != nil || node != "prod-7" {
		t.Errorf("singleNode = %q, %+v", node, perr)
	}
}

func TestOneLineKeepsMessagesEvidenceSafe(t *testing.T) {
	got := oneLine("first \"quoted\" line\nsecond line")
	if strings.ContainsAny(got, "\"\n") {
		t.Errorf("oneLine left quotes or newlines in %q", got)
	}
	if want := "first 'quoted' line"; got != want {
		t.Errorf("oneLine = %q, want %q", got, want)
	}
}

// closedTo makes a path unreadable for the rest of the test. Root reads a
// mode-000 path regardless, so the test is skipped there rather than
// asserting something the filesystem is not doing.
func closedTo(t *testing.T, path string, restore os.FileMode) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 path")
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(path, restore); err != nil {
			t.Errorf("restore the mode: %v", err)
		}
	})
}

// wantRefused fails the test unless the refusal carries the code and the
// words that name what went wrong.
func wantRefused(t *testing.T, perr *protoError, code, message string) {
	t.Helper()
	if perr == nil || perr.Code != code || !strings.Contains(perr.Message, message) {
		t.Errorf("got %+v, want %s mentioning %q", perr, code, message)
	}
}

// TestTheArchiveKindRefusesTheShapesAnOperatorGetsWrong: the archive is
// judged by the sandbox's tar and then by the engine, so the host only
// refuses what it can see without reading content — and an empty file is
// a failed backup job rather than an archive.
func TestTheArchiveKindRefusesTheShapesAnOperatorGetsWrong(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.tar")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	backup := writeBackupFixture(t, t.TempDir(), "nightly", backupSpec{})
	for name, tc := range map[string]struct {
		path, code, message string
	}{
		"a path that does not exist": {
			filepath.Join(dir, "gone.tar"), "source_not_found", "does not exist",
		},
		"a directory":    {backup, "invalid_request", "use kind weaviate_backup"},
		"an empty file":  {empty, "source_corrupt", "is empty"},
		"beneath a file": {filepath.Join(empty, "nightly.tar"), "source_unreadable", "stat backup source"},
	} {
		t.Run(name, func(t *testing.T) {
			_, perr := resolveSource("weaviate_backup_tar", tc.path, nil)
			wantRefused(t, perr, tc.code, tc.message)
		})
	}
}

// TestWhatTheHostCannotReadIsUnreadable: the host hashes the artifact and
// reads the backup's own metadata before anything moves. A read that
// fails is the host's failure, never a verdict about the backup.
func TestWhatTheHostCannotReadIsUnreadable(t *testing.T) {
	t.Run("an archive the host may not open", func(t *testing.T) {
		archive := tarOf(t, writeBackupFixture(t, t.TempDir(), "nightly", backupSpec{}))
		closedTo(t, archive, 0o600)
		_, perr := resolveSource("weaviate_backup_tar", archive, nil)
		wantRefused(t, perr, "source_unreadable", "read backup source")
	})
	t.Run("a chunk the host may not open", func(t *testing.T) {
		backup := writeBackupFixture(t, t.TempDir(), "nightly", backupSpec{})
		closedTo(t, filepath.Join(backup, metaFileName), 0o600)
		_, perr := resolveSource("weaviate_backup", backup, nil)
		wantRefused(t, perr, "source_unreadable", "read "+metaFileName)
	})
	t.Run("a directory of backups the host may not list", func(t *testing.T) {
		parent := t.TempDir()
		writeBackupFixture(t, parent, "nightly", backupSpec{})
		closedTo(t, parent, 0o755)
		_, perr := resolveSource("weaviate_backup_dir", parent, nil)
		wantRefused(t, perr, "source_unreadable", "")
	})
	t.Run("bytes that will not stream", func(t *testing.T) {
		_, perr := fileChecksum(t.TempDir())
		wantRefused(t, perr, "source_unreadable", "read backup source")
	})
}

// TestADirectoryOfBackupsSaysWhyItHasNoWinner: "nothing found" is never
// the whole story — an attempt that is still running, a directory holding
// entries that are not backups, and an empty directory each read
// differently to an operator.
func TestADirectoryOfBackupsSaysWhyItHasNoWinner(t *testing.T) {
	t.Run("a directory with nothing in it", func(t *testing.T) {
		_, perr := resolveSource("weaviate_backup_dir", t.TempDir(), nil)
		wantRefused(t, perr, "source_not_found", "contains no files")
	})
	t.Run("a directory holding no backup", func(t *testing.T) {
		parent := t.TempDir()
		if err := os.MkdirAll(filepath.Join(parent, "logs"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(parent, "notes.txt"), []byte("not a backup\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, perr := resolveSource("weaviate_backup_dir", parent, nil)
		wantRefused(t, perr, "source_not_found", "were passed over")
	})
}

// TestAnUnparseableInstantNeverWins: a backup ranks by the instant it
// states about itself, and one that states something unreadable ranks as
// the zero instant rather than as now.
func TestAnUnparseableInstantNeverWins(t *testing.T) {
	if got := parseInstant("the third of September"); !got.IsZero() {
		t.Errorf("parseInstant = %v, want the zero instant", got)
	}
	if got := parseInstant(fixtureCompletedAt); got.IsZero() {
		t.Errorf("parseInstant(%q) = zero, want the instant it states", fixtureCompletedAt)
	}
}

// TestAStatusTheBackupDidNotStateIsSaidAsSuch: a refusal quoting an empty
// status would read as a bug in the adapter rather than as a backup that
// never said.
func TestAStatusTheBackupDidNotStateIsSaidAsSuch(t *testing.T) {
	if got := orUnstated("  "); got != "(unstated)" {
		t.Errorf("orUnstated = %q, want (unstated)", got)
	}
	if got := orUnstated("TRANSFERRING"); got != "TRANSFERRING" {
		t.Errorf("orUnstated = %q, want the status the backup states", got)
	}
}

// TestTheTreeChecksumMeasuresEveryPartOfTheArtifact: the hash reaches the
// evidence record as the backup's identity, so it covers the chunks
// themselves, it sees a link as its own target, and a chunk the host
// cannot read stops it rather than quietly hashing less.
func TestTheTreeChecksumMeasuresEveryPartOfTheArtifact(t *testing.T) {
	spec := backupSpec{}.withDefaults("nightly")
	chunk := filepath.Join("nightly", spec.node, spec.classes[0], "chunk-1")

	t.Run("a changed chunk changes the identity", func(t *testing.T) {
		parent := t.TempDir()
		backup := writeBackupFixture(t, parent, "nightly", backupSpec{})
		before, _, perr := dirChecksum(backup)
		if perr != nil {
			t.Fatalf("dirChecksum: %+v", perr)
		}
		if err := os.WriteFile(filepath.Join(parent, chunk), []byte("other chunk bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
		after, _, perr := dirChecksum(backup)
		if perr != nil || after == before {
			t.Errorf("checksum %s unchanged after a chunk changed (%+v)", after, perr)
		}
	})
	t.Run("a link is hashed as the link it is", func(t *testing.T) {
		backup := writeBackupFixture(t, t.TempDir(), "nightly", backupSpec{})
		link := filepath.Join(backup, "latest")
		if err := os.Symlink(spec.node, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		before, _, perr := dirChecksum(backup)
		if perr != nil {
			t.Fatalf("dirChecksum: %+v", perr)
		}
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("somewhere-else", link); err != nil {
			t.Fatal(err)
		}
		after, _, perr := dirChecksum(backup)
		if perr != nil || after == before {
			t.Errorf("checksum %s unchanged after the link moved (%+v)", after, perr)
		}
	})
	t.Run("a chunk the host may not open", func(t *testing.T) {
		parent := t.TempDir()
		backup := writeBackupFixture(t, parent, "nightly", backupSpec{})
		closedTo(t, filepath.Join(parent, chunk), 0o600)
		_, _, perr := dirChecksum(backup)
		wantRefused(t, perr, "source_unreadable", "")
	})
}
