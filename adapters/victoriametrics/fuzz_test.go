package main

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzParseBackupMetadata drives the marker reader over arbitrary bytes.
//
// backup_metadata.ignore is the artifact's own statement of when the
// snapshot froze, and this adapter acts on it three times: it ranks the
// candidates in a backup directory, it becomes the record's
// backup.created_at, and it is the instant the series census queries the
// restored engine at (ops.go). An artifact therefore chooses all three,
// which is why the value has to be a plausible instant before any of
// them: the sibling adapters that read an epoch out of an artifact
// (prometheus, cassandra) gate it the same way.
func FuzzParseBackupMetadata(f *testing.F) {
	f.Add([]byte(`{"created_at":"2026-08-14T14:37:45Z","completed_at":"2026-08-14T14:38:02Z"}`))
	f.Add([]byte(`{"created_at":"2026-08-14T14:37:45+02:00"}`))
	f.Add([]byte(`{"created_at":"1970-01-01T00:00:00Z"}`))
	f.Add([]byte(`{"created_at":"0000-01-01T00:00:00Z"}`))
	f.Add([]byte(`{"created_at":""}`))
	f.Add([]byte(`{}`))
	f.Add([]byte("not json"))
	f.Add([]byte(""))

	f.Fuzz(func(t *testing.T, raw []byte) {
		ms, ok := parseBackupMetadata(raw)

		if !ok {
			if ms != 0 {
				t.Fatalf("parseBackupMetadata reported nothing readable and returned %d", ms)
			}
			return
		}
		if ms == 0 {
			t.Fatal("a success returned 0, which every caller reads as 'the artifact does not date itself'")
		}
		if !plausibleEpochMs(ms) {
			t.Fatalf("created_at %d is not an instant a snapshot was taken at, yet it picks the "+
				"backup to restore and the time the checks evaluate at", ms)
		}
	})
}

// FuzzDeclaredParts drives the parts list reader over arbitrary bytes,
// and asks the fence about every name it produces.
//
// The two together are the completeness check: parts.json states what a
// partition requires, and the census refuses a backup that does not hold
// what its own metadata names — before a byte is transferred. The reader
// is deliberately lenient (it does not grade the engine's metadata
// format), so the fence is what must not be talked past: a name that
// resolves anywhere but inside the partition would let a truncated copy
// satisfy the census with a directory the backup does not contain.
func FuzzDeclaredParts(f *testing.F) {
	partition := filepath.Join(f.TempDir(), "partition")
	for _, dir := range []string{filepath.Join(partition, "1_2_3_ABC"), filepath.Join(partition, "sub", "nested")} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			f.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(partition, "plain.bin"), nil, 0o600); err != nil {
		f.Fatal(err)
	}

	f.Add([]byte(`["1_2_3_ABC"]`))
	f.Add([]byte(`{"Small":["1_2_3_ABC"],"Big":["sub/nested"]}`))
	f.Add([]byte(`["../partition/1_2_3_ABC"]`))
	f.Add([]byte(`["",".","..","plain.bin"]`))
	f.Add([]byte(`{}`))
	f.Add([]byte("not json"))

	f.Fuzz(func(t *testing.T, raw []byte) {
		for _, name := range declaredParts(raw) {
			if !partDirExists(partition, name) {
				continue
			}
			if name != filepath.Base(name) || name == "." || name == ".." {
				t.Fatalf("the census accepted part %q, which does not name an entry of the partition", name)
			}
			info, err := os.Stat(filepath.Join(partition, name))
			if err != nil || !info.IsDir() {
				t.Fatalf("the census accepted part %q, which is not a directory here: %v", name, err)
			}
		}
	})
}
