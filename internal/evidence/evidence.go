// Package evidence implements Probavi's tamper-evident drill evidence log:
// an append-only JSONL file of hash-chained, ed25519-signed records, as
// specified in docs/evidence-schema.md (probavi-evidence/1). That document
// is normative; this package follows it byte for byte. The stored line of
// every record IS its canonical serialization (RFC 8785 JCS restricted to
// integer-only numbers); signing and hashing operate on those bytes and
// nothing else. The writer emits the current schema version; verification
// accepts every published version, each against its own shape.
package evidence

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
)

const (
	// SchemaID is the evidence schema version this package writes.
	SchemaID = "probavi-evidence/3"

	// SchemaIDv0 is the first published version: v1 without
	// drill.pitr_target.
	SchemaIDv0 = "probavi-evidence/0"

	// SchemaIDv1 is the second: v2 without adapter.digest and
	// env.probavi_digest. Records declaring either earlier version verify
	// forever (evidence-schema.md §10); they are never rewritten or
	// re-signed.
	SchemaIDv1 = "probavi-evidence/1"

	// SchemaIDv2 is the third: v3 without the backup manifest verdict, the
	// restored data's newest instant, what the sandbox actually was, and
	// the host's clock belief.
	SchemaIDv2 = "probavi-evidence/2"

	// GenesisPrevHash is the prev_hash of the first record in a chain.
	GenesisPrevHash = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

	// MaxRecordBytes is the maximum canonical size of a stored record line.
	MaxRecordBytes = 64 * 1024

	// MaxSafeInteger bounds every number in a record (|n| <= 2^53 - 1).
	MaxSafeInteger = 1<<53 - 1

	maxDetailLen       = 256
	maxErrorMessageLen = 512
)

// Sentinel errors returned by this package. Callers match with errors.Is.
var (
	ErrNotInteger     = errors.New("number is not a safe integer")
	ErrNotCanonical   = errors.New("stored bytes are not canonical")
	ErrRecordTooLarge = errors.New("record exceeds maximum canonical size")
	ErrInvalidRecord  = errors.New("record violates the evidence schema")
	ErrKeyPermissions = errors.New("key file permissions too open")
	ErrKeyFormat      = errors.New("malformed key file")
	ErrUnknownKey     = errors.New("no public key for key_id")
	ErrLocked         = errors.New("evidence log is locked by another writer")
	ErrChainState     = errors.New("existing log fails chain validation")
)

// lineHash returns the "sha256:<hex>" reference of a stored record line
// (canonical bytes, without the trailing newline).
func lineHash(line []byte) string {
	sum := sha256.Sum256(line)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// SchemaIDs returns every published schema version this package verifies,
// oldest first. Verification and the generated capabilities manifest read
// the same list, so what the binary accepts and what the manifest claims
// cannot drift apart.
func SchemaIDs() []string {
	return []string{SchemaIDv0, SchemaIDv1, SchemaIDv2, SchemaID}
}

// supportedSchema reports whether a stored record's declared schema version
// is one this verifier implements (evidence-schema.md §10).
func supportedSchema(s string) bool {
	return slices.Contains(SchemaIDs(), s)
}
