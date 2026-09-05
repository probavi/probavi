package main

import (
	"testing"
)

// FuzzParseManifestBytes drives the backup manifest reader over
// arbitrary bytes.
//
// An `influx backup` directory states what it holds in a JSON manifest,
// and this adapter acts on that statement before a byte is transferred:
// it refuses the 1.x portable format and unverified manifest versions,
// and it resolves the members the backup must contain. A manifest read
// wrongly either fails a good backup or, worse, lets an incomplete one
// through — so a success has to carry everything the member resolution
// then relies on.
func FuzzParseManifestBytes(f *testing.F) {
	f.Add([]byte(`{"manifestVersion":2,"kv":{"fileName":"kv.bolt","size":4},"buckets":[{"organizationName":"o","bucketName":"b","retentionPolicies":[{"shardGroups":[{"shards":[{"fileName":"1.tar.gz"}]}]}]}]}`))
	f.Add([]byte(`{"manifestVersion":2,"kv":{"fileName":"kv.bolt"},"sql":{"fileName":"sql.bolt"},"buckets":[{"bucketName":"b"}]}`))
	f.Add([]byte(`{"meta":{"fileName":"meta.00"},"buckets":[{"bucketName":"b"}]}`))
	f.Add([]byte(`{"manifestVersion":3,"kv":{"fileName":"kv.bolt"},"buckets":[{"bucketName":"b"}]}`))
	f.Add([]byte(`{"kv":{"fileName":""},"buckets":[{"bucketName":"b"}]}`))
	f.Add([]byte(`{"kv":{"fileName":"kv.bolt"},"buckets":[]}`))
	f.Add([]byte(`{}`))
	f.Add([]byte("not json"))
	f.Add([]byte(""))

	f.Fuzz(func(t *testing.T, raw []byte) {
		m, perr := parseManifestBytes(raw, "20260814T143745Z.manifest")

		if perr != nil {
			if m != nil {
				t.Fatalf("refusal %q still returned a manifest", perr.Code)
			}
			return
		}
		if m.KV == nil || m.KV.FileName == "" {
			t.Fatal("a success returned a manifest naming no KV store, which member resolution dereferences")
		}
		if len(m.Buckets) == 0 {
			t.Fatal("a success returned a manifest naming no buckets, which is a refusal")
		}
		if m.ManifestVersion != nil && *m.ManifestVersion != manifestVersionVerified {
			t.Fatalf("a success returned manifest version %d, which this adapter is not verified against",
				*m.ManifestVersion)
		}
		assertMemberNames(t, m)
	})
}

// assertMemberNames holds the member list to what memberFiles relies on:
// every name is matched against the directory's own entries, so an empty
// one would resolve to the backup directory itself, and a duplicate would
// transfer the same member twice.
func assertMemberNames(t *testing.T, m *backupManifest) {
	t.Helper()
	seen := map[string]bool{}
	for _, name := range manifestMemberNames(m) {
		if name == "" {
			t.Fatal("member list carries an empty name, which resolves to the backup directory itself")
		}
		if seen[name] {
			t.Fatalf("member %q listed twice — the list is documented as free of duplicates", name)
		}
		seen[name] = true
	}
}
