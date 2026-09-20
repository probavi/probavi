package main

import (
	"archive/tar"
	"bytes"
	"testing"
)

// FuzzRecordManifest drives the dump manifest reader over arbitrary
// bytes.
//
// dump.json is what a dump says about itself: when it was taken, and
// which database it came from. The first dates an evidence record and the
// second decides where the restore goes, so a hostile or corrupt manifest
// must be unable to do either. The reader is tolerant by design — one it
// cannot parse leaves the dump unclaimed — so what it must never do is
// record something it did not read.
func FuzzRecordManifest(f *testing.F) {
	f.Add([]byte(`{"database":"shop","createdAt":"2026-09-20T14:21:39Z","useEnvelope":false}`))
	f.Add([]byte(`{"database":"","createdAt":""}`))
	f.Add([]byte(`{"createdAt":"0000-01-01T00:00:00Z"}`))
	f.Add([]byte(`{"createdAt":"9999-12-31T23:59:59Z"}`))
	f.Add([]byte(`{"database":"../../etc"}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`not json at all`))
	f.Add([]byte(``))

	f.Fuzz(func(t *testing.T, raw []byte) {
		facts := newDumpFacts()
		recordManifest(facts, raw)
		if !facts.manifestOK && facts.createdMs != 0 {
			t.Fatal("a manifest that did not parse still dated the backup")
		}
		if facts.createdMs != 0 && !plausibleEpochMs(facts.createdMs) {
			t.Fatalf("createdMs = %d, outside what a running engine could write", facts.createdMs)
		}
		// Whatever it read, the verdict must be a decision rather than a
		// panic — and a database name it would accept must be one that
		// can go into a command line unchanged.
		perr := judgeDump(facts)
		if perr == nil && !identifierShape.MatchString(facts.manifest.Database) {
			t.Fatalf("accepted database name %q, which is not usable unquoted",
				facts.manifest.Database)
		}
	})
}

// FuzzWalkTar drives the archive walk over arbitrary bytes.
//
// A backup file is attacker-controlled input (SECURITY.md). The walk is a
// bonus pass on the host, so on anything it cannot read it must say
// nothing and let the sandbox's own tar be the authority — what it must
// never do is produce a census it did not read.
func FuzzWalkTar(f *testing.F) {
	f.Add(wholeDumpTar(f))
	f.Add([]byte("not a tar at all"))
	f.Add([]byte(""))

	f.Fuzz(func(t *testing.T, raw []byte) {
		census, verdict, ok := walkTar(tar.NewReader(bytes.NewReader(raw)))
		if !ok {
			if verdict != nil || census.database != "" || len(census.collections) != 0 {
				t.Fatal("a walk that read nothing still produced a claim")
			}
			return
		}
		if verdict != nil && (census.database != "" || len(census.collections) != 0) {
			t.Fatal("a refusal came with a census; a verdict is one or the other")
		}
		if verdict == nil && !identifierShape.MatchString(census.database) {
			t.Fatalf("census names database %q, which is not usable unquoted", census.database)
		}
		if census.createdMs != 0 && !plausibleEpochMs(census.createdMs) {
			t.Fatalf("createdMs = %d, outside what a running engine could write", census.createdMs)
		}
	})
}

// wholeDumpTar builds the one seed that is a real dump archive, so the
// corpus starts from something the walk accepts rather than only from
// bytes it rejects.
func wholeDumpTar(f *testing.F) []byte {
	f.Helper()
	buf := &bytes.Buffer{}
	tw := tar.NewWriter(buf)
	entries := map[string][]byte{
		manifestName:   []byte(`{"database":"shop","createdAt":"2026-09-20T14:21:39Z"}`),
		encryptionName: []byte("none"),
		"orders_" + "0123456789abcdef0123456789abcdef" + ".structure.json": []byte(`{"indexes":[]}`),
		"orders_" + "0123456789abcdef0123456789abcdef" + ".data.json.gz":   []byte("gz"),
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
