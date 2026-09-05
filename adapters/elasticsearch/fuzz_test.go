package main

import (
	"testing"
)

// FuzzParseRepoIndex drives the repository generation reader over
// arbitrary bytes.
//
// index-<gen> is the repository's own list of what it holds, read on the
// drill host out of an artifact nothing has vetted. What it returns
// becomes the census the restore is judged against and the snapshot
// names quoted back to the operator, so a success must mean a census
// with something in it — the empty repository is a refusal, not a
// census.
func FuzzParseRepoIndex(f *testing.F) {
	f.Add([]byte(`{"snapshots":[{"name":"snap-1","uuid":"u1","state":1,"index_version":9}],"indices":{"books":{"id":"i1","snapshots":["u1"]}}}`))
	f.Add([]byte(`{"snapshots":[],"indices":{}}`))
	f.Add([]byte(`{"snapshots":[{"name":""}]}`))
	f.Add([]byte(`{"indices":{"books":{}}}`))
	f.Add([]byte(`{}`))
	f.Add([]byte("not json"))
	f.Add([]byte(""))

	f.Fuzz(func(t *testing.T, raw []byte) {
		census, perr := parseRepoIndex(raw)

		if perr != nil {
			if len(census.snapshots) != 0 || len(census.indices) != 0 {
				t.Fatalf("refusal %q still returned a census of %d snapshots and %d indices",
					perr.Code, len(census.snapshots), len(census.indices))
			}
			return
		}
		if len(census.snapshots) == 0 {
			t.Fatal("a success returned no snapshots, which is the one shape parseRepoIndex refuses")
		}
		if len(census.names()) != len(census.snapshots) {
			t.Fatalf("names() gave %d for %d snapshots", len(census.names()), len(census.snapshots))
		}
	})
}
