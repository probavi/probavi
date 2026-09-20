package main

import (
	"archive/tar"
	"bytes"
	"testing"
)

// FuzzRecordManifest drives the snapshot manifest reader over arbitrary
// bytes.
//
// manifest.json is what a snapshot says about itself: when it was taken,
// and which sstables it should hold. The first dates an evidence record
// and the second decides whether the copy is called complete, so a
// hostile or corrupt manifest must be unable to do either. The reader is
// deliberately tolerant — one it cannot parse leaves the table judged by
// its files alone — so what it must never do is record a claim it did
// not read.
func FuzzRecordManifest(f *testing.F) {
	f.Add([]byte(`{"snapshot":{"name":"drill","created_at":1789900285},` +
		`"table":{"keyspace_name":"shop","table_name":"orders","tablet_count":16},` +
		`"sstables":[{"toc_name":"mt-a-big-TOC.txt","data_size":1144,"tablet_id":0}]}`))
	f.Add([]byte(`{"snapshot":{"created_at":-1}}`))
	f.Add([]byte(`{"snapshot":{"created_at":99999999999999}}`))
	f.Add([]byte(`{"sstables":[{"toc_name":""}]}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`not json at all`))
	f.Add([]byte(``))

	f.Fuzz(func(t *testing.T, raw []byte) {
		facts := newTableFacts()
		recordManifest(facts, raw)
		if !facts.manifestOK && facts.createdMs != 0 {
			t.Fatal("a manifest that did not parse still dated the backup")
		}
		if facts.createdMs != 0 && !plausibleEpochMs(facts.createdMs) {
			t.Fatalf("createdMs = %d, which is outside what a running engine could write",
				facts.createdMs)
		}
		// Whatever it read, the completeness verdict must be a decision
		// rather than a panic, and it must not claim a file is present
		// that the walk never saw.
		facts.hasSchema = true
		facts.manifestOK = true
		if perr := judgeComponents(tableRef{"shop", "orders"}, facts); perr != nil &&
			perr.Code != "source_corrupt" {
			t.Fatalf("completeness verdict = %q, want source_corrupt or nothing", perr.Code)
		}
	})
}

// FuzzWalkTar drives the archive walk over arbitrary bytes.
//
// A backup file is attacker-controlled input (SECURITY.md). The walk is a
// bonus pass on the host, so on anything it cannot read it must say
// nothing and let the sandbox's own tar be the authority — what it must
// never do is produce a census it did not read, or spend memory the
// archive chose.
func FuzzWalkTar(f *testing.F) {
	f.Add(wholeSnapshotTar(f))
	f.Add([]byte("not a tar at all"))
	f.Add([]byte(""))

	f.Fuzz(func(t *testing.T, raw []byte) {
		census, verdict, ok := walkTar(tar.NewReader(bytes.NewReader(raw)))
		if !ok {
			if verdict != nil || len(census.tables) != 0 {
				t.Fatal("a walk that read nothing still produced a claim")
			}
			return
		}
		if verdict != nil && len(census.tables) != 0 {
			t.Fatal("a refusal came with a census; a verdict is one or the other")
		}
		if census.maxCreatedMs != 0 && !plausibleEpochMs(census.maxCreatedMs) {
			t.Fatalf("maxCreatedMs = %d, outside what a running engine could write",
				census.maxCreatedMs)
		}
		for _, ref := range census.tables {
			if !identifierShape.MatchString(ref.keyspace) || !identifierShape.MatchString(ref.table) {
				t.Fatalf("census carries %s, which cannot go into CQL unquoted", ref)
			}
		}
	})
}

// wholeSnapshotTar builds the one seed that is a real snapshot archive,
// so the corpus starts from something the walk accepts rather than only
// from bytes it rejects.
func wholeSnapshotTar(f *testing.F) []byte {
	f.Helper()
	buf := &bytes.Buffer{}
	tw := tar.NewWriter(buf)
	entries := map[string][]byte{
		"shop/orders/" + manifestName: []byte(`{"snapshot":{"created_at":1789900285},"sstables":[]}`),
		"shop/orders/" + schemaName:   []byte("CREATE TABLE shop.orders (id int PRIMARY KEY);\n"),
	}
	for name, body := range entries {
		hdr := &tar.Header{Name: name, Typeflag: tar.TypeReg, Size: int64(len(body))}
		if err := tw.WriteHeader(hdr); err != nil {
			f.Fatalf("seed header: %v", err)
		}
		if _, err := tw.Write(body); err != nil {
			f.Fatalf("seed body: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		f.Fatalf("seed close: %v", err)
	}
	return buf.Bytes()
}
