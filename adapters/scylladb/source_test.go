package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tarOf packs a directory tree into a tar archive, optionally under one
// wrapping directory — both shapes an operator's `tar -cf` produces.
func tarOf(t *testing.T, root, prefix string) string {
	t.Helper()
	buf := &bytes.Buffer{}
	tw := tar.NewWriter(buf)
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil || rel == "." {
			return err
		}
		name := filepath.ToSlash(rel)
		if prefix != "" {
			name = prefix + "/" + name
		}
		if d.IsDir() {
			return tw.WriteHeader(&tar.Header{Name: name + "/", Typeflag: tar.TypeDir, Mode: 0o755})
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(body)),
		}); err != nil {
			return err
		}
		_, err = tw.Write(body)
		return err
	})
	if err != nil {
		t.Fatalf("pack tar: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	path := filepath.Join(t.TempDir(), "snapshot.tar")
	writeFile(t, path, buf.String())
	return path
}

// wantRefusal drives resolveSource and asserts the code and a phrase of
// the message, because a refusal an operator cannot act on is only half
// a refusal.
func wantRefusal(t *testing.T, kind, path, code, phrase string) {
	t.Helper()
	_, perr := resolveSource(kind, path)
	if perr == nil {
		t.Fatalf("resolveSource(%s, %s) succeeded, want %s", kind, path, code)
	}
	if perr.Code != code {
		t.Errorf("code = %q, want %q (message: %s)", perr.Code, code, perr.Message)
	}
	if !strings.Contains(perr.Message, phrase) {
		t.Errorf("message = %q, want it to mention %q", perr.Message, phrase)
	}
}

func TestATreeIsReadForWhatItStatesAboutItself(t *testing.T) {
	root := t.TempDir()
	writeSnapshot(t, root,
		snapshotTable{keyspace: "shop", table: "orders", createdAt: 1789900285, sstables: 2},
		snapshotTable{keyspace: "shop", table: "items", createdAt: 1789900100, sstables: 1},
	)
	src, perr := resolveSource("scylladb_snapshot", root)
	if perr != nil {
		t.Fatalf("resolveSource: %+v", perr)
	}
	if len(src.census.tables) != 2 {
		t.Fatalf("tables = %v, want both", src.census.tables)
	}
	// Sorted, so a restore order is deterministic and the connection's
	// database is a stable choice.
	if src.census.tables[0].table != "items" {
		t.Errorf("tables = %v, want them sorted", src.census.tables)
	}
	// The newest instant any manifest claims, not the oldest.
	if want := int64(1789900285) * 1000; src.census.maxCreatedMs != want {
		t.Errorf("maxCreatedMs = %d, want %d", src.census.maxCreatedMs, want)
	}
	if src.sizeBytes <= 0 || !strings.HasPrefix(src.checksum, "sha256:") {
		t.Errorf("identity = %q/%d, want a real measurement", src.checksum, src.sizeBytes)
	}
	if src.tarball {
		t.Error("a tree was resolved as an archive")
	}
}

func TestAnArchiveIsWalkedOnTheHost(t *testing.T) {
	for name, prefix := range map[string]string{
		"keyspaces at the root":        "",
		"under one wrapping directory": "backup-2026-09-20",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeSnapshot(t, root, oneTable())
			src, perr := resolveSource("scylladb_snapshot_tar", tarOf(t, root, prefix))
			if perr != nil {
				t.Fatalf("resolveSource: %+v", perr)
			}
			if !src.tarball {
				t.Error("an archive was not resolved as one")
			}
			if len(src.census.tables) != 1 || src.census.tables[0].String() != "shop.orders" {
				t.Errorf("census = %v, want shop.orders read out of the archive", src.census.tables)
			}
			if src.census.maxCreatedMs == 0 {
				t.Error("the archive's own manifest instant was not read")
			}
		})
	}
}

// TestAStreamThatIsNotTarShapedSaysNothing: the host's pass is a bonus.
// Where it cannot read the stream it must not produce a verdict — the
// sandbox's own tar is then the authority.
func TestAStreamThatIsNotTarShapedSaysNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notatar.tar")
	writeFile(t, path, "this is not a tar archive at all\n")
	src, perr := resolveSource("scylladb_snapshot_tar", path)
	if perr != nil {
		t.Fatalf("resolveSource refused an unreadable stream: %+v — the sandbox decides", perr)
	}
	if len(src.census.tables) != 0 {
		t.Errorf("census = %v, want nothing claimed about a stream that was not read", src.census.tables)
	}
	if !strings.HasPrefix(src.checksum, "sha256:") {
		t.Error("the artifact was not hashed")
	}
}

// TestTheManifestIsTheCompletenessGate is the measurement this adapter
// leans on: the engine writes one sstable per tablet and names each set
// in manifest.json, so a copy that lost one is detectable before a byte
// is restored — which a directory listing cannot see, because what
// remains still looks like a snapshot.
func TestTheManifestIsTheCompletenessGate(t *testing.T) {
	root := t.TempDir()
	writeSnapshot(t, root, snapshotTable{
		keyspace: "shop", table: "orders", createdAt: 1789900285, sstables: 3, omitData: true,
	})
	wantRefusal(t, "scylladb_snapshot", root, "source_corrupt", "its own manifest.json lists")

	// The same gate through the archive path.
	tarRoot := t.TempDir()
	writeSnapshot(t, tarRoot, snapshotTable{
		keyspace: "shop", table: "orders", createdAt: 1789900285, sstables: 3, omitData: true,
	})
	wantRefusal(t, "scylladb_snapshot_tar", tarOf(t, tarRoot, ""), "source_corrupt", "incomplete")
}

func TestWhatASnapshotMustCarry(t *testing.T) {
	for name, tc := range map[string]struct {
		table  snapshotTable
		code   string
		phrase string
	}{
		"no schema": {
			snapshotTable{keyspace: "shop", table: "orders", createdAt: 1789900285, sstables: 1, omitSchema: true},
			"source_corrupt", "holds no schema.cql",
		},
		"no manifest": {
			snapshotTable{keyspace: "shop", table: "orders", createdAt: 1789900285, sstables: 1, omitManifest: true},
			"source_corrupt", "holds no readable manifest.json",
		},
		"a live data directory": {
			snapshotTable{keyspace: "shop", table: "orders", createdAt: 1789900285, sstables: 1, live: "snapshots"},
			"invalid_request", "live data directory",
		},
		"the engine's own keyspace": {
			snapshotTable{keyspace: "system_schema", table: "tables", createdAt: 1789900285, sstables: 1},
			"invalid_request", "one of the engine's own keyspaces",
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeSnapshot(t, root, tc.table)
			wantRefusal(t, "scylladb_snapshot", root, tc.code, tc.phrase)
		})
	}
}

// TestANameThatCannotGoIntoCQLIsRefused: directory names reach composed
// CQL and argv, so anything that is not an unquoted identifier is refused
// rather than quoted around.
func TestANameThatCannotGoIntoCQLIsRefused(t *testing.T) {
	for _, bad := range []string{"Orders", "orders;drop", "1shop", "shop-1", ""} {
		root := t.TempDir()
		dir := filepath.Join(root, "shop", bad)
		if bad == "" {
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		_, perr := resolveSource("scylladb_snapshot", root)
		if perr == nil || perr.Code != "invalid_request" {
			t.Errorf("table name %q was accepted (%v), want it refused", bad, perr)
		}
	}
}

func TestTheDirectoryKindPicksWhatTheManifestsDate(t *testing.T) {
	root := t.TempDir()
	for name, created := range map[string]int64{
		"monday": 1789900000, "tuesday": 1789999999, "sunday": 1789800000,
	} {
		sub := filepath.Join(root, name)
		writeSnapshot(t, sub, snapshotTable{
			keyspace: "shop", table: "orders", createdAt: created, sstables: 1,
		})
		// File times run the other way, so a ranking by mtime would pick
		// the wrong one — which is exactly the mistake being avoided.
		old := time.Now().Add(-time.Duration(created%1000) * time.Hour)
		if err := os.Chtimes(sub, old, old); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}
	src, perr := resolveSource("scylladb_snapshot_dir", root)
	if perr != nil {
		t.Fatalf("resolveSource: %+v", perr)
	}
	if filepath.Base(src.path) != "tuesday" {
		t.Errorf("chose %q, want the tree whose own manifest claims the newest instant", src.path)
	}
}

// TestADatedTreeOutranksAnUndatedOne: the drill would rather restore the
// backup it can also say something true about.
func TestADatedTreeOutranksAnUndatedOne(t *testing.T) {
	dated := treeCandidate{name: "a", maxCreatedMs: 1}
	undated := treeCandidate{name: "z", mtime: time.Now()}
	if !dated.beats(undated) {
		t.Error("an undated tree outranked a dated one")
	}
	if undated.beats(dated) {
		t.Error("the ranking is not antisymmetric")
	}
	// Two undated candidates fall back to directory time, then to name.
	older := treeCandidate{name: "b", mtime: time.Unix(1000, 0)}
	newer := treeCandidate{name: "a", mtime: time.Unix(2000, 0)}
	if !newer.beats(older) {
		t.Error("undated candidates did not fall back to directory time")
	}
	same := treeCandidate{name: "a", mtime: older.mtime}
	if !older.beats(same) {
		t.Error("the name tiebreak is not deterministic")
	}
}

func TestTheKindsAndWhatTheyRefuse(t *testing.T) {
	root := t.TempDir()
	writeSnapshot(t, root, oneTable())
	file := tarOf(t, root, "")

	wantRefusal(t, "scylladb_snapshot_tar", root, "invalid_request", "is a directory")
	wantRefusal(t, "scylladb_snapshot", file, "invalid_request", "is a file")
	wantRefusal(t, "scylladb_snapshot", filepath.Join(root, "nope"), "source_not_found", "does not exist")
	wantRefusal(t, "scylladb_snapshot_tar", filepath.Join(root, "nope.tar"), "source_not_found", "does not exist")
	wantRefusal(t, "scylladb_snapshot_dir", filepath.Join(root, "nope"), "source_not_found", "does not exist")
	wantRefusal(t, "mysqldump", file, "unsupported_source", "unsupported source kind")
	wantRefusal(t, "scylladb_snapshot_dir", t.TempDir(), "source_not_found", "contains no snapshot trees")
	wantRefusal(t, "scylladb_snapshot", t.TempDir(), "source_corrupt", "not a collected snapshot")
}

// TestADeclaredTimezoneIsRefusedRatherThanIgnored: the manifest states an
// epoch instant, so the parameter cannot improve anything, and silence
// would leave the operator believing it did.
func TestADeclaredTimezoneIsRefusedRatherThanIgnored(t *testing.T) {
	if perr := rejectBackupTimezone(nil); perr != nil {
		t.Errorf("no parameter was refused: %+v", perr)
	}
	if perr := rejectBackupTimezone(map[string]string{backupTimezoneParam: ""}); perr != nil {
		t.Errorf("an empty parameter was refused: %+v", perr)
	}
	perr := rejectBackupTimezone(map[string]string{backupTimezoneParam: "Europe/Budapest"})
	if perr == nil || perr.Code != "invalid_request" {
		t.Fatalf("a declared zone was accepted: %+v", perr)
	}
	if !strings.Contains(perr.Message, "epoch seconds") {
		t.Errorf("message = %q, want it to say why the parameter is redundant", perr.Message)
	}
}

func TestAnInstantOutsideTheRangeDoesNotDateARecord(t *testing.T) {
	for name, tc := range map[string]struct {
		createdAt int64
		want      bool
	}{
		"a real instant":    {1789900285, true},
		"zero":              {0, false},
		"before the engine": {100000, false},
		"far in the future": {99999999999, false},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeSnapshot(t, root, snapshotTable{
				keyspace: "shop", table: "orders", createdAt: tc.createdAt, sstables: 1,
			})
			src, perr := resolveSource("scylladb_snapshot", root)
			if perr != nil {
				t.Fatalf("resolveSource: %+v", perr)
			}
			if got := src.census.maxCreatedMs != 0; got != tc.want {
				t.Errorf("dated = %v, want %v for created_at %d", got, tc.want, tc.createdAt)
			}
		})
	}
}

func TestFormatCreatedAtRendersUTCMilliseconds(t *testing.T) {
	if got := formatCreatedAt(0); got != nil {
		t.Errorf("formatCreatedAt(0) = %v, want nil", *got)
	}
	got := formatCreatedAt(1789900285123)
	if got == nil {
		t.Fatal("formatCreatedAt returned nil for a real instant")
	}
	if !strings.HasSuffix(*got, "Z") || !strings.Contains(*got, ".123") {
		t.Errorf("created_at = %q, want UTC with milliseconds", *got)
	}
}

// TestAnArchiveCannotChooseHowMuchMemoryTheHostSpends: a tar entry is a
// 512-byte header that compresses to almost nothing, and a backup file is
// attacker-controlled input.
func TestAnArchiveCannotChooseHowMuchMemoryTheHostSpends(t *testing.T) {
	r := retention{}
	if !r.take(10) {
		t.Fatal("a first small entry was refused")
	}
	r.bytes = keptMaxBytes
	if r.take(1) {
		t.Error("the byte bound does not hold")
	}
	r2 := retention{entries: keptMaxEntries}
	if r2.take(1) {
		t.Error("the entry bound does not hold")
	}
	if perr := tooMuchKept(); perr == nil || perr.Code != "source_corrupt" {
		t.Errorf("tooMuchKept = %+v, want a source_corrupt refusal", perr)
	}
}

func TestSplitTarNameDropsEmptyAndDotSegments(t *testing.T) {
	for in, want := range map[string]string{
		"shop/orders/schema.cql":   "shop,orders,schema.cql",
		"./shop/orders/schema.cql": "shop,orders,schema.cql",
		"/shop//orders/schema.cql": "shop,orders,schema.cql",
		"schema.cql":               "schema.cql",
	} {
		if got := strings.Join(splitTarName(in), ","); got != want {
			t.Errorf("splitTarName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestAManifestThatDoesNotParseIsNotAManifest: a corrupt one must leave
// the table unclaimed rather than silently dated.
func TestAManifestThatDoesNotParseIsNotAManifest(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "shop", "orders")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(dir, schemaName), "CREATE TABLE shop.orders (id int PRIMARY KEY);\n")
	writeFile(t, filepath.Join(dir, manifestName), "{not json at all")
	wantRefusal(t, "scylladb_snapshot", root, "source_corrupt", "holds no readable manifest.json")
}

// TestAManifestListingNothingIsStillComplete: a table nobody ever wrote
// to contributes no sstable, and that is legitimate.
func TestAManifestListingNothingIsStillComplete(t *testing.T) {
	root := t.TempDir()
	writeSnapshot(t, root, snapshotTable{
		keyspace: "shop", table: "orders", createdAt: 1789900285, sstables: 0,
	})
	src, perr := resolveSource("scylladb_snapshot", root)
	if perr != nil {
		t.Fatalf("an empty but whole table was refused: %+v", perr)
	}
	if len(src.census.tables) != 1 {
		t.Errorf("census = %v, want the table counted", src.census.tables)
	}
}

func TestFileChecksumRefusesWhatItCannotRead(t *testing.T) {
	if _, perr := fileChecksum(filepath.Join(t.TempDir(), "absent")); perr == nil ||
		perr.Code != "source_unreadable" {
		t.Errorf("fileChecksum of a missing file = %+v, want source_unreadable", perr)
	}
}

func TestTheManifestShapeIsTheOneTheEngineWrites(t *testing.T) {
	// Pinned against a real manifest.json, so a field rename upstream
	// fails here rather than quietly dropping created_at.
	const real = `{"manifest":{"version":"1.0","scope":"node"},` +
		`"node":{"host_id":"4cddcf5c-350c-4f32-99a2-f10927d12ccd","datacenter":"datacenter1"},` +
		`"snapshot":{"name":"pvtest","created_at":1789900285},` +
		`"table":{"keyspace_name":"probavi","table_name":"orders","tablet_count":16},` +
		`"sstables":[{"id":"6e4d5fe0","toc_name":"mt-a-big-TOC.txt","data_size":1144,"tablet_id":15}]}`
	m := scyllaManifest{}
	if err := json.Unmarshal([]byte(real), &m); err != nil {
		t.Fatalf("the engine's own manifest does not parse: %v", err)
	}
	if m.Snapshot.CreatedAt != 1789900285 {
		t.Errorf("created_at = %d, want the epoch seconds the engine wrote", m.Snapshot.CreatedAt)
	}
	if m.Table.TabletCount != 16 || len(m.SSTables) != 1 {
		t.Errorf("table/sstables = %+v/%d, want the shape measured", m.Table, len(m.SSTables))
	}
	if m.SSTables[0].TOCName != "mt-a-big-TOC.txt" || m.SSTables[0].DataSize != 1144 {
		t.Errorf("sstable entry = %+v, want its toc_name and size", m.SSTables[0])
	}
	if got := fmt.Sprint(m.Table.KeyspaceName, ".", m.Table.TableName); got != "probavi.orders" {
		t.Errorf("table names = %q", got)
	}
}
