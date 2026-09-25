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
)

// TestTheReadinessLoopWaitsRatherThanGivingUp: the node answers when it
// answers, and a first refusal is not a failed drill.
func TestTheReadinessLoopWaitsRatherThanGivingUp(t *testing.T) {
	root := t.TempDir()
	writeSnapshot(t, root, oneTable())
	sim := defaultSimulated()
	var sequence []string
	base := provisionHandler(t, &sequence, sim)
	refusals := 2
	line, _, _ := driveOp(t, "provision",
		provisionPayload(t, "scylladb_snapshot", root, nil),
		func(call verbCall) (any, *protoError) {
			if call.Verb == "exec" {
				argv := argvOf(t, call)
				if argv[0] == "cqlsh" && strings.Contains(strings.Join(argv, " "), "release_version") &&
					refusals > 0 {
					refusals--
					sequence = append(sequence, "not-ready")
					return errExec(1, "Connection refused"), nil
				}
			}
			return base(call)
		})
	if f := parseFinal(t, line); !f.OK {
		t.Fatalf("provision failed while the node was still coming up: %+v", f.Error)
	}
	if refusals != 0 {
		t.Errorf("the loop gave up after %d refusals", 2-refusals)
	}
	if !contains(sequence, "not-ready") || !contains(sequence, "ready") {
		t.Errorf("the flow did not wait and then proceed: %v", sequence)
	}
}

// TestTheVersionCheckServiceIsNotWorthADrill: stopping the phone-home is
// best effort, because an image without supervisord is not an error.
func TestTheVersionCheckServiceIsNotWorthADrill(t *testing.T) {
	root := t.TempDir()
	writeSnapshot(t, root, oneTable())
	sim := defaultSimulated()
	var sequence []string
	base := provisionHandler(t, &sequence, sim)
	line, _, _ := driveOp(t, "provision",
		provisionPayload(t, "scylladb_snapshot", root, nil),
		func(call verbCall) (any, *protoError) {
			if call.Verb == "exec" {
				argv := argvOf(t, call)
				if len(argv) > 2 && strings.Contains(argv[2], "supervisorctl stop") {
					return errExec(127, "supervisorctl: not found"), nil
				}
			}
			return base(call)
		})
	if f := parseFinal(t, line); !f.OK {
		t.Fatalf("an image without supervisorctl failed the drill: %+v", f.Error)
	}
}

// TestTheCompletenessGateNamesWhatIsMissing, including the TOC itself and
// the case where more files are gone than a message should list.
func TestTheCompletenessGateNamesWhatIsMissing(t *testing.T) {
	facts := newTableFacts()
	facts.hasSchema = true
	facts.manifestOK = true
	for i := range 6 {
		facts.manifest.SSTables = append(facts.manifest.SSTables, struct {
			TOCName  string `json:"toc_name"`
			DataSize int64  `json:"data_size"`
			TabletID *int   `json:"tablet_id"`
		}{TOCName: fmt.Sprintf("mt-%d-big%s", i, tocSuffix)})
	}
	ref := tableRef{keyspace: "shop", table: "orders"}
	perr := judgeComponents(ref, facts)
	if perr == nil {
		t.Fatal("a table missing every file was accepted")
	}
	if !strings.Contains(perr.Message, "missing 6 of the 6") {
		t.Errorf("message = %q, want the count of what is gone", perr.Message)
	}
	// The message lists a few names, not all of them.
	if strings.Count(perr.Message, "-big-TOC.txt") > 3 {
		t.Errorf("message = %q, want at most three names shown", perr.Message)
	}

	// A TOC present but its Data.db gone is the half-copied shape.
	facts2 := newTableFacts()
	facts2.hasSchema, facts2.manifestOK = true, true
	facts2.manifest.SSTables = facts.manifest.SSTables[:1]
	facts2.entries[facts.manifest.SSTables[0].TOCName] = true
	if perr := judgeComponents(ref, facts2); perr == nil ||
		!strings.Contains(perr.Message, "-Data.db") {
		t.Errorf("a missing Data.db was not named: %+v", perr)
	}

	// An entry with no toc_name is skipped rather than blamed.
	facts3 := newTableFacts()
	facts3.hasSchema, facts3.manifestOK = true, true
	facts3.manifest.SSTables = append(facts3.manifest.SSTables, struct {
		TOCName  string `json:"toc_name"`
		DataSize int64  `json:"data_size"`
		TabletID *int   `json:"tablet_id"`
	}{})
	if perr := judgeComponents(ref, facts3); perr != nil {
		t.Errorf("a nameless manifest entry produced a verdict: %+v", perr)
	}
}

// TestAnArchiveOfALiveDataDirectoryIsNamedAsOne: the tar path has to
// recognise it too, not only the filesystem walk.
func TestAnArchiveOfALiveDataDirectoryIsNamedAsOne(t *testing.T) {
	root := t.TempDir()
	writeSnapshot(t, root, snapshotTable{
		keyspace: "shop", table: "orders", createdAt: 1789900285, sstables: 1, live: "upload",
	})
	wantRefusal(t, "scylladb_snapshot_tar", tarOf(t, root, ""), "invalid_request", "live data directory")
}

// TestATarEntryThatIsNotAFileIsIgnored: symlinks and devices carry no
// claim, and a shallow path is not a table.
func TestATarEntryThatIsNotAFileIsIgnored(t *testing.T) {
	buf := &bytes.Buffer{}
	tw := tar.NewWriter(buf)
	write := func(h *tar.Header, body string) {
		h.Size = int64(len(body))
		if err := tw.WriteHeader(h); err != nil {
			t.Fatalf("header: %v", err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("body: %v", err)
		}
	}
	write(&tar.Header{Name: "loose.txt", Typeflag: tar.TypeReg, Mode: 0o600}, "x")
	write(&tar.Header{Name: "shop/orders/link", Typeflag: tar.TypeSymlink, Linkname: "elsewhere"}, "")
	write(&tar.Header{Name: "Shop/Orders/schema.cql", Typeflag: tar.TypeReg, Mode: 0o600}, "x")
	if err := tw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	path := filepath.Join(t.TempDir(), "odd.tar")
	writeFile(t, path, buf.String())

	// shop/orders collected only a symlink, so it has no schema and no
	// manifest: the walk produces a verdict rather than a census.
	_, perr := resolveSource("scylladb_snapshot_tar", path, nil)
	if perr == nil {
		t.Fatal("an archive holding no usable table was accepted with a census")
	}
	if perr.Code != "source_corrupt" {
		t.Errorf("code = %q, want source_corrupt", perr.Code)
	}
}

// TestAnArchiveWholeEnoughToWalkCarriesItsInstant pins the bonus the host
// pass provides when the stream is readable.
func TestAnArchiveWholeEnoughToWalkCarriesItsInstant(t *testing.T) {
	root := t.TempDir()
	writeSnapshot(t, root,
		snapshotTable{keyspace: "shop", table: "orders", createdAt: 1789900285, sstables: 1},
		snapshotTable{keyspace: "shop", table: "items", createdAt: 1789900999, sstables: 1},
	)
	src, perr := resolveSource("scylladb_snapshot_tar", tarOf(t, root, ""), nil)
	if perr != nil {
		t.Fatalf("resolveSource: %+v", perr)
	}
	if want := int64(1789900999) * 1000; src.census.maxCreatedMs != want {
		t.Errorf("maxCreatedMs = %d, want the newest of the two", src.census.maxCreatedMs)
	}
	if len(src.census.tables) != 2 || src.census.tables[0].table != "items" {
		t.Errorf("tables = %v, want both, sorted", src.census.tables)
	}
}

func TestSortTablesOrdersByKeyspaceThenTable(t *testing.T) {
	tables := []tableRef{
		{"shop", "orders"}, {"acme", "z"}, {"shop", "items"}, {"acme", "a"},
	}
	sortTables(tables)
	got := make([]string, len(tables))
	for i, r := range tables {
		got[i] = r.String()
	}
	want := "acme.a,acme.z,shop.items,shop.orders"
	if strings.Join(got, ",") != want {
		t.Errorf("sorted = %v, want %s", got, want)
	}
}

func TestReadCappedStopsAtTheCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.json")
	writeFile(t, path, strings.Repeat("x", metaMaxBytes+4096))
	raw, err := readCapped(path)
	if err != nil {
		t.Fatalf("readCapped: %v", err)
	}
	if len(raw) != metaMaxBytes {
		t.Errorf("read %d bytes, want the cap of %d", len(raw), metaMaxBytes)
	}
	if _, err := readCapped(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("a missing file read without error")
	}
}

// TestAKeyspaceDirectoryTheHostCannotReadIsUnreadable, rather than being
// reported as a malformed backup.
func TestAKeyspaceDirectoryTheHostCannotReadIsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every directory")
	}
	root := t.TempDir()
	ks := filepath.Join(root, "shop")
	if err := os.MkdirAll(filepath.Join(ks, "orders"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(ks, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(ks, 0o755); err != nil {
			t.Errorf("restore permissions: %v", err)
		}
	})
	_, perr := resolveSource("scylladb_snapshot", root, nil)
	if perr == nil || perr.Code != "source_unreadable" {
		t.Errorf("verdict = %+v, want source_unreadable", perr)
	}
}

func TestATableDirectoryTheHostCannotReadIsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every directory")
	}
	root := t.TempDir()
	tbl := filepath.Join(root, "shop", "orders")
	if err := os.MkdirAll(tbl, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(tbl, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(tbl, 0o755); err != nil {
			t.Errorf("restore permissions: %v", err)
		}
	})
	_, perr := resolveSource("scylladb_snapshot", root, nil)
	if perr == nil || perr.Code != "source_unreadable" {
		t.Errorf("verdict = %+v, want source_unreadable", perr)
	}
}

// TestTheStagedTreeMirrorsTheArtifact: every file is put, under the same
// relative path, so the sandbox sees what the host read.
func TestTheStagedTreeMirrorsTheArtifact(t *testing.T) {
	root := t.TempDir()
	writeSnapshot(t, root, oneTable())
	dirs, files, perr := walkTree(root, "/stage")
	if perr != nil {
		t.Fatalf("walkTree: %+v", perr)
	}
	if len(dirs) < 3 {
		t.Errorf("dirs = %v, want the root plus keyspace and table", dirs)
	}
	if dirs[0] != "/stage" {
		t.Errorf("dirs[0] = %q, want the stage root first", dirs[0])
	}
	var sawSchema bool
	for _, f := range files {
		if !strings.HasPrefix(f.dest, "/stage/shop/orders/") {
			t.Errorf("dest = %q, want it under the staged table directory", f.dest)
		}
		if strings.HasSuffix(f.dest, schemaName) {
			sawSchema = true
		}
	}
	if !sawSchema {
		t.Error("the schema was not staged")
	}
	if _, _, perr := walkTree(filepath.Join(root, "absent"), "/stage"); perr == nil {
		t.Error("walking a missing directory produced no verdict")
	}
}

// TestAManifestWithAnAbsurdInstantDoesNotDateTheRecord guards the
// narrower unit directly: a hostile manifest must not choose what an
// evidence record says a backup's age is.
func TestAManifestWithAnAbsurdInstantDoesNotDateTheRecord(t *testing.T) {
	for _, ms := range []int64{0, -1, 1, 1420070400000, 4102444800000, 1 << 62} {
		if plausibleEpochMs(ms) {
			t.Errorf("plausibleEpochMs(%d) = true, want it refused", ms)
		}
	}
	if !plausibleEpochMs(1789900285000) {
		t.Error("a real instant was refused")
	}
}

func TestRecordManifestKeepsOnlyWhatParses(t *testing.T) {
	f := newTableFacts()
	recordManifest(f, []byte("{not json"))
	if f.manifestOK {
		t.Error("a manifest that does not parse was accepted")
	}
	raw, err := json.Marshal(map[string]any{
		"snapshot": map[string]any{"created_at": 1789900285},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	recordManifest(f, raw)
	if !f.manifestOK || f.createdMs == 0 {
		t.Errorf("a real manifest was not recorded: ok=%v created=%d", f.manifestOK, f.createdMs)
	}
}
