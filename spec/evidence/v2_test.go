package evidence

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

// recordSchema is the published JSON Schema, which this package's tests
// read but its code never does: the verifier was written from
// docs/evidence-schema.md and stays free of dependencies. The schema is a
// normative artifact of the same specification, so pinning against it is
// pinning against the spec, not against the core.
const recordSchema = "../../docs/schemas/evidence/record.json"

// publishedSchemaVersions returns every version the JSON Schema declares a
// branch for, by walking oneOf → $defs → properties.schema.const.
func publishedSchemaVersions(t *testing.T) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(recordSchema)
	if err != nil {
		t.Fatalf("read %s: %v", recordSchema, err)
	}
	var doc struct {
		OneOf []struct {
			Ref string `json:"$ref"`
		} `json:"oneOf"`
		Defs map[string]struct {
			Properties struct {
				Schema struct {
					Const string `json:"const"`
				} `json:"schema"`
			} `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse %s: %v", recordSchema, err)
	}
	out := make(map[string]bool)
	for _, branch := range doc.OneOf {
		name := strings.TrimPrefix(branch.Ref, "#/$defs/")
		def, ok := doc.Defs[name]
		if !ok {
			t.Fatalf("%s: oneOf references %q, which $defs does not define", recordSchema, name)
		}
		if def.Properties.Schema.Const == "" {
			t.Fatalf("%s: branch %q pins no schema identifier", recordSchema, name)
		}
		out[def.Properties.Schema.Const] = true
	}
	if len(out) == 0 {
		t.Fatalf("%s: no version branches — this gate would pass vacuously", recordSchema)
	}
	return out
}

// TestSupportedSchemasMatchThePublishedVersions is the §10 obligation as a
// test: a verifier MUST support every published version, for the lifetime
// of the format.
//
// Nothing enforced that. The specification could publish a version this
// package refuses — which is how an auditor ends up holding a log the
// independent verifier calls INVALID for no reason but a stale allow-list.
// Dropping a version is caught in the same breath, and that would be worse:
// records already written would stop verifying.
func TestSupportedSchemasMatchThePublishedVersions(t *testing.T) {
	published := publishedSchemaVersions(t)
	for version := range published {
		if !supportedSchemas[version] {
			t.Errorf("%s publishes %s, which this verifier refuses — §10 obliges it to accept "+
				"every published version", recordSchema, version)
		}
	}
	for version := range supportedSchemas {
		if !published[version] {
			t.Errorf("this verifier accepts %s, which %s does not publish", version, recordSchema)
		}
	}
}

// exampleSigner rebuilds the deterministic key pair the published
// conformance vectors were signed with (§11: seed bytes 0x00…0x1f), so a
// test can produce a record the committed public key verifies. The private
// half is a fixture, never a real key.
func exampleSigner(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		t.Fatal("ed25519 private key did not yield a public key")
	}
	if !pub.Equal(exampleKey(t)) {
		t.Fatal("the derived key pair is not the one the committed examples were signed with")
	}
	return priv
}

// signRecord completes a record the way §6 requires: canonicalize with the
// sig member absent, sign those bytes, then attach sig and canonicalize
// again to get the stored line.
func signRecord(t *testing.T, rec map[string]any, priv ed25519.PrivateKey, keyID string) []byte {
	t.Helper()
	delete(rec, "sig")
	message, err := canonicalize(rec)
	if err != nil {
		t.Fatalf("canonicalize the signed message: %v", err)
	}
	rec["sig"] = map[string]any{
		"alg":     "ed25519",
		"key_id":  keyID,
		"sig_b64": base64.StdEncoding.EncodeToString(ed25519.Sign(priv, message)),
	}
	return canonLine(t, rec)
}

// objOf returns a record's sub-object with a checked assertion, so fixture
// drift surfaces as a failure rather than a nil-map panic.
func objOf(t *testing.T, rec map[string]any, key string) map[string]any {
	t.Helper()
	obj, ok := rec[key].(map[string]any)
	if !ok {
		t.Fatalf("fixture has no %s object", key)
	}
	return obj
}

// chainHash is §5's prev_hash of a stored line: SHA-256 over the line
// without its terminator, which is what the verifier hashes.
func chainHash(storedLine []byte) string {
	sum := sha256.Sum256(storedLine[:len(storedLine)-1])
	return "sha256:" + hex.EncodeToString(sum[:])
}

// v2Record is the shape §3 documents for probavi-evidence/2: v1 plus the
// two digests. The verifier checks the schema identifier, canonical bytes,
// chaining and signature — not the field inventory, which is the JSON
// Schema's job — so this fixture is what a real v2 writer will produce.
func v2Record(seq int, prevHash string, adapterDigest, coreDigest any) map[string]any {
	return map[string]any{
		"schema":    "probavi-evidence/2",
		"seq":       json.Number(fmt.Sprint(seq)),
		"prev_hash": prevHash,
		"ts":        "2026-08-05T09:00:00.000Z",
		"drill": map[string]any{
			"name":        "prod-orders-db",
			"config_hash": "sha256:" + strings.Repeat("7d", 32),
			"pitr_target": nil,
		},
		"backup": map[string]any{
			"kind":       "pgdump",
			"checksum":   "sha256:" + strings.Repeat("9f", 32),
			"size_bytes": json.Number("565248"),
			"created_at": "2026-08-05T08:00:00.000Z",
		},
		"adapter": map[string]any{
			"name":     "postgres",
			"version":  "0.3.0",
			"protocol": "probavi-adapter/0",
			"digest":   adapterDigest,
		},
		"sandbox":    map[string]any{"provider": "docker", "params": map[string]any{"image": "postgres:16"}},
		"timings_ms": map[string]any{"total": json.Number("2840")},
		"checks":     []any{},
		"outcome":    "pass",
		"error":      nil,
		"env": map[string]any{
			"probavi_version": "0.2.0",
			"os":              "linux",
			"arch":            "amd64",
			"host_id":         "3f7a9c2e5b1d8e04",
			"probavi_digest":  coreDigest,
		},
	}
}

// TestV2RecordsVerify proves the version bump is real rather than an entry
// in a map: a v2 record, signed the way §6 prescribes, verifies with only
// the committed public key — the position an auditor is in. The null case
// matters as much as the populated one, because §3 makes both digests
// nullable so that an unreadable executable never costs a drill its record.
func TestV2RecordsVerify(t *testing.T) {
	priv := exampleSigner(t)
	kr := NewKeyring(exampleKey(t))
	keyID := KeyID(exampleKey(t))

	tests := []struct {
		name          string
		adapter, core any
	}{
		{"both digests present", "sha256:" + strings.Repeat("4c", 32), "sha256:" + strings.Repeat("1d", 32)},
		{"both digests null", nil, nil},
		{"adapter unreadable, core recorded", nil, "sha256:" + strings.Repeat("1d", 32)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line := signRecord(t, v2Record(1, genesisPrevHash, tt.adapter, tt.core), priv, keyID)
			res := verifyBytes(t, line, kr)
			if res.Status != StatusValid {
				t.Fatalf("status = %s (%s), want VALID", res.Status, res.Reason)
			}
			if res.Records != 1 {
				t.Errorf("records = %d, want 1", res.Records)
			}
		})
	}
}

// TestV2ChainsAcrossVersions covers the §10 case a real upgrade produces: a
// log whose writer moved from v1 to v2 mid-file. Each record is validated
// against its own declared version, and the chain runs straight through.
func TestV2ChainsAcrossVersions(t *testing.T) {
	priv := exampleSigner(t)
	kr := NewKeyring(exampleKey(t))
	keyID := KeyID(exampleKey(t))

	first := v2Record(1, genesisPrevHash, nil, nil)
	first["schema"] = "probavi-evidence/1"
	delete(objOf(t, first, "adapter"), "digest")
	delete(objOf(t, first, "env"), "probavi_digest")
	line1 := signRecord(t, first, priv, keyID)

	digest := "sha256:" + strings.Repeat("4c", 32)
	line2 := signRecord(t, v2Record(2, chainHash(line1), digest, digest), priv, keyID)

	res := verifyBytes(t, append(append([]byte(nil), line1...), line2...), kr)
	if res.Status != StatusValid {
		t.Fatalf("status = %s (%s at line %d), want VALID", res.Status, res.Reason, res.Line)
	}
	if res.Records != 2 {
		t.Errorf("records = %d, want 2", res.Records)
	}
}

// TestUnpublishedVersionStillRefused keeps the allow-list load-bearing: it
// grows an entry at a time, never into a wildcard. The version it names
// moves with each bump — it is always the next one nothing has published.
func TestUnpublishedVersionStillRefused(t *testing.T) {
	priv := exampleSigner(t)
	rec := v3Record(t, 1, genesisPrevHash)
	rec["schema"] = "probavi-evidence/4"
	line := signRecord(t, rec, priv, KeyID(exampleKey(t)))

	res := verifyBytes(t, line, NewKeyring(exampleKey(t)))
	if res.Status != StatusInvalid {
		t.Fatalf("status = %s, want INVALID", res.Status)
	}
	if !strings.Contains(res.Reason, `unsupported schema "probavi-evidence/4"`) {
		t.Errorf("reason = %q, want the unsupported-schema rejection", res.Reason)
	}
}

// v3Record is the §3 shape of probavi-evidence/3: v2 plus the backup
// manifest verdict, the restored data's newest instant, what the sandbox
// actually was, and the host's clock belief.
func v3Record(t *testing.T, seq int, prevHash string) map[string]any {
	t.Helper()
	rec := v2Record(seq, prevHash,
		"sha256:"+strings.Repeat("05", 32), "sha256:"+strings.Repeat("1d", 32))
	rec["schema"] = "probavi-evidence/3"
	backup := objOf(t, rec, "backup")
	backup["manifest_hash"] = "sha256:" + strings.Repeat("3b", 32)
	backup["manifest_match"] = true
	backup["newest_data_at"] = "2026-08-05T07:59:42.000Z"
	rec["sandbox"] = map[string]any{
		"provider":     "docker",
		"params":       map[string]any{"image": "postgres:16"},
		"image_digest": "sha256:" + strings.Repeat("9f", 32),
		"resources": map[string]any{
			"memory_bytes": json.Number("2147483648"),
			"cpus_milli":   json.Number("1500"),
		},
	}
	objOf(t, rec, "env")["clock_synchronised"] = true
	return rec
}

// v3NullRecord is the same version with every v3 field null, which §3
// makes legal so that a drill naming no backup manifest, on a provider
// that cannot read back its limits, on a host with no time daemon, still
// emits a conforming record. A verifier that only ever saw the populated
// shape would not be verifying the version.
func v3NullRecord(t *testing.T, seq int, prevHash string) map[string]any {
	t.Helper()
	rec := v3Record(t, seq, prevHash)
	backup := objOf(t, rec, "backup")
	backup["manifest_hash"], backup["manifest_match"], backup["newest_data_at"] = nil, nil, nil
	sandbox := objOf(t, rec, "sandbox")
	sandbox["image_digest"] = nil
	sandbox["resources"] = map[string]any{"memory_bytes": nil, "cpus_milli": nil}
	objOf(t, rec, "env")["clock_synchronised"] = nil
	return rec
}

// TestV3RecordsVerify proves the bump is real rather than an entry in a
// map: both shapes §3 allows, signed the way §6 prescribes, verify with
// only the committed public key — the position an auditor is in.
func TestV3RecordsVerify(t *testing.T) {
	priv := exampleSigner(t)
	kr := NewKeyring(exampleKey(t))
	keyID := KeyID(exampleKey(t))

	for name, build := range map[string]func(*testing.T, int, string) map[string]any{
		"every field populated": v3Record,
		"every field null":      v3NullRecord,
	} {
		t.Run(name, func(t *testing.T) {
			line := signRecord(t, build(t, 1, genesisPrevHash), priv, keyID)
			res := verifyBytes(t, line, kr)
			if res.Status != StatusValid || res.Records != 1 {
				t.Fatalf("status = %s, records = %d, reason = %q; want VALID", res.Status, res.Records, res.Reason)
			}
		})
	}
}

// TestV2ToV3ChainRunsStraightThrough: §10 permits one file to hold records
// of different versions, because an upgrade happens mid-file. The chain is
// over canonical bytes and knows nothing of versions, and this is what
// proves it.
func TestV3ChainFollowsAV2Record(t *testing.T) {
	priv := exampleSigner(t)
	keyID := KeyID(exampleKey(t))

	line1 := signRecord(t, v2Record(1, genesisPrevHash, nil, nil), priv, keyID)
	line2 := signRecord(t, v3Record(t, 2, chainHash(line1)), priv, keyID)

	res := verifyBytes(t, append(append([]byte(nil), line1...), line2...), NewKeyring(exampleKey(t)))
	if res.Status != StatusValid || res.Records != 2 {
		t.Fatalf("status = %s, records = %d, reason = %q; want VALID over both", res.Status, res.Records, res.Reason)
	}
}
