package evidence

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// fuzz_test.go fuzzes the independent verifier — the implementation a third
// party runs when they have the format specification, this module, and no
// reason to trust the core.
//
// The seeds are the published conformance vectors, and they are the same
// files the core's own fuzz targets are seeded from. The two implementations
// cannot import one another (that isolation is the point of this module, and
// the toolchain enforces it), so agreement is established by feeding both the
// same corpus rather than by a differential test.

// publishedExamples returns the worked-example logs and the key that signed
// them, straight from the schema's published vectors.
func publishedExamples(t testing.TB) (logs [][]byte, keys Keyring) {
	t.Helper()
	dir := filepath.Join("..", "..", "docs", "schemas", "evidence", "examples")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read examples: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		logs = append(logs, raw)
	}
	if len(logs) == 0 {
		t.Fatal("no published example logs — the seed corpus would be empty")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "signer.pub"))
	if err != nil {
		t.Fatalf("read published key: %v", err)
	}
	pub, err := ParsePublicKey(raw)
	if err != nil {
		t.Fatalf("parse published key: %v", err)
	}
	return logs, NewKeyring(pub)
}

// FuzzVerify drives the verifier over arbitrary bytes and asserts that its
// verdict describes itself consistently. An auditor acts on this answer and
// has nothing else to cross-check it against, so a verdict that contradicts
// its own fields is a defect even when the classification happens to be
// right.
func FuzzVerify(f *testing.F) {
	logs, keys := publishedExamples(f)
	for _, raw := range logs {
		f.Add(raw)
	}
	f.Add([]byte(""))
	f.Add([]byte("\n"))
	f.Add([]byte("{}"))
	f.Add([]byte(`{"schema":"probavi-evidence/2","seq":1}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		res, err := Verify(bytes.NewReader(data), keys)
		if err != nil {
			t.Fatalf("Verify returned an I/O error over an in-memory reader: %v", err)
		}
		assertVerdictIsSelfConsistent(t, res)
	})
}

// assertVerdictIsSelfConsistent checks a Result against the meaning of its
// own status, so the same rules apply to any verdict however it arose.
func assertVerdictIsSelfConsistent(t *testing.T, res Result) {
	t.Helper()
	switch res.Status {
	case StatusValid:
		if len(res.DamagedLines) != 0 {
			t.Fatalf("VALID with damaged lines %v: %+v", res.DamagedLines, res)
		}
		if res.Line != 0 || res.Reason != "" {
			t.Fatalf("VALID carrying a rejection: %+v", res)
		}
	case StatusValidDamage:
		if len(res.DamagedLines) == 0 {
			t.Fatalf("VALID_WITH_DAMAGE naming no damaged line: %+v", res)
		}
	case StatusInvalid:
		if res.Line == 0 {
			t.Fatalf("INVALID naming no line: %+v", res)
		}
		if res.Reason == "" {
			t.Fatalf("INVALID with no reason: %+v", res)
		}
	default:
		t.Fatalf("unknown status %q", res.Status)
	}
	if res.Records < 0 {
		t.Fatalf("negative record count: %+v", res)
	}
	for _, line := range res.DamagedLines {
		if line <= 0 {
			t.Fatalf("damaged line number %d is not 1-based: %+v", line, res)
		}
	}
}

// FuzzVerifyWithoutKeys pins the answer a verifier must give before anyone
// has handed it a key: never that records are authentic. A verifier that
// accepted records without a key would make the signatures decorative, and
// this module exists precisely so that claim is checked by code nobody who
// wrote the signer also wrote.
func FuzzVerifyWithoutKeys(f *testing.F) {
	logs, _ := publishedExamples(f)
	for _, raw := range logs {
		f.Add(raw)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		res, err := Verify(bytes.NewReader(data), Keyring{})
		if err != nil {
			t.Fatalf("Verify returned an I/O error over an in-memory reader: %v", err)
		}
		if res.Status == StatusValid && res.Records > 0 {
			t.Fatalf("records verified with an empty keyring: %+v", res)
		}
	})
}

// FuzzParsePublicKey covers the one piece of operator-supplied input the
// verifier reads besides the log itself: the key file. It is read before any
// verdict exists, so a crash here is a refusal to verify at all.
func FuzzParsePublicKey(f *testing.F) {
	f.Add([]byte(""))
	f.Add([]byte("not hex"))
	f.Add(bytes.Repeat([]byte("ab"), 32))
	f.Add(append(bytes.Repeat([]byte("ab"), 32), '\n'))
	f.Add(bytes.Repeat([]byte("AB"), 32))

	f.Fuzz(func(t *testing.T, data []byte) {
		pub, err := ParsePublicKey(data)
		if err != nil {
			return
		}
		if len(pub) == 0 {
			t.Fatalf("accepted %q as a key and returned nothing usable", data)
		}
		// A key the parser accepted must be usable to build a keyring: the
		// next thing every caller does with it.
		if len(NewKeyring(pub)) != 1 {
			t.Fatalf("accepted key does not make a keyring: %q", data)
		}
	})
}
