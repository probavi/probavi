package main

import (
	"archive/tar"
	"bytes"
	"path"
	"sort"
	"testing"
)

// tarSeed builds one archive out of name/body pairs, for seeding the
// fuzzer with inputs that are tar-shaped to begin with.
func tarSeed(t testing.TB, entries ...string) []byte {
	t.Helper()
	buf := &bytes.Buffer{}
	tw := tar.NewWriter(buf)
	for i := 0; i+1 < len(entries); i += 2 {
		name, body := entries[i], entries[i+1]
		hdr := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// FuzzScanBackupTar drives the host-side archive walk over arbitrary
// bytes.
//
// This pass is the fence: it reads the operator's backup file on the
// drill host, before anything is transferred, to find a configset that
// would delete the documents the drill restores. The file is
// attacker-controlled input (SECURITY.md) and the walk is what stands
// between it and the host.
//
// The properties are the ones the walk promises its caller. The
// retention bound is the load-bearing one — a tar entry is a 512-byte
// header, so without it a small archive chooses how much memory the
// drill host spends. The rest keep the refusal honest: what comes back
// as `expiring` is quoted verbatim to the operator as the reason their
// drill was refused, so every name in it has to be a configuration file
// the walk actually read, and a refusal must not also claim to have
// identified a collection.
func FuzzScanBackupTar(f *testing.F) {
	const expiring = `<updateRequestProcessorChain><processor class="solr.` + expirationClass + `"/></updateRequestProcessorChain>`

	f.Add(tarSeed(f, "snapshot.mycoll/mycoll/backup_0.properties", "startTime=x"))
	f.Add(tarSeed(f, "mycoll/conf/"+solrConfigFile, expiring))
	f.Add(tarSeed(f, "mycoll/conf/"+solrConfigFile, "<config/>"))
	f.Add(tarSeed(f,
		"a/backup_0.properties", "", "b/backup_0.properties", "",
		"a/conf/"+solrConfigFile, expiring))
	f.Add(tarSeed(f, "../../etc/"+solrConfigFile, expiring))
	f.Add([]byte("not a tar at all"))
	f.Add([]byte(""))

	f.Fuzz(func(t *testing.T, archive []byte) {
		collection, found, perr := scanBackupTar(bytes.NewReader(archive), "backup.tar")

		if perr != nil && collection != "" {
			t.Fatalf("refusal %q also named collection %q", perr.Code, collection)
		}
		if len(found) > keptMaxEntries {
			t.Fatalf("kept %d names, over the %d the walk may retain", len(found), keptMaxEntries)
		}
		total := 0
		for _, name := range found {
			total += len(name)
			if path.Base(name) != solrConfigFile {
				t.Fatalf("expiring names %q, which is not a %s — the refusal quotes these to the operator",
					name, solrConfigFile)
			}
		}
		if total > keptMaxBytes {
			t.Fatalf("kept %d bytes of names, over the %d the walk may retain", total, keptMaxBytes)
		}
		if !sort.StringsAreSorted(found) {
			t.Fatalf("expiring = %q is not sorted", found)
		}
	})
}
