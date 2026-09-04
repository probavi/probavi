package evidence

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Signer signs evidence records with an ed25519 private key.
type Signer struct {
	key   ed25519.PrivateKey
	keyID string
}

// maxKeyFileBytes bounds what a key file may be read into memory as. A
// seed is 64 hex characters and a newline; the rest of the allowance is
// so a file with trailing whitespace still reaches the format error that
// names the real problem, rather than a size error that does not.
const maxKeyFileBytes = 4 << 10

// LoadSigner reads a signing key from a file holding the 32-byte seed as 64
// lowercase hex characters. It refuses key files readable by group or
// other (evidence-schema.md §6).
//
// The file is opened once and every question asked of the descriptor. A
// stat followed by a read is two lookups of one name, and what is
// permission-checked need not be what is read; the difference matters
// here more than most places, because what is read becomes the key that
// signs every record.
//
// The open is non-blocking and the mode is checked, so a path that is not
// a regular file is refused rather than obeyed. Both failures are
// reachable by a typo: a FIFO makes a plain read wait for a writer that
// never comes, and a character device like /dev/zero makes it read until
// the host runs out of memory. Neither is a key.
func LoadSigner(path string) (*Signer, error) {
	path = filepath.Clean(path)
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open key file: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only descriptor
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat key file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrKeyFormat, path)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("%w: %s has mode %04o, want 0600 or stricter", ErrKeyPermissions, path, perm)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxKeyFileBytes))
	if err != nil {
		return nil, fmt.Errorf("read key file: %w", err)
	}
	seed, err := decodeKeyHex(raw, ed25519.SeedSize)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return NewSignerFromSeed(seed), nil
}

// NewSignerFromSeed builds a Signer from a raw 32-byte ed25519 seed.
func NewSignerFromSeed(seed []byte) *Signer {
	key := ed25519.NewKeyFromSeed(seed)
	return &Signer{key: key, keyID: PublicKeyID(publicKeyOf(key))}
}

// KeyID returns the key identifier records signed by this Signer carry.
func (s *Signer) KeyID() string { return s.keyID }

// PublicKey returns the verification key matching this Signer.
func (s *Signer) PublicKey() ed25519.PublicKey {
	return publicKeyOf(s.key)
}

func publicKeyOf(key ed25519.PrivateKey) ed25519.PublicKey {
	pub, ok := key.Public().(ed25519.PublicKey)
	if !ok {
		// ed25519.PrivateKey.Public is documented to return ed25519.PublicKey.
		panic("evidence: unexpected ed25519 public key type")
	}
	return pub
}

func (s *Signer) sign(message []byte) []byte {
	return ed25519.Sign(s.key, message)
}

// PublicKeyID derives a key_id: the first 16 hex characters of SHA-256 over
// the 32 raw public-key bytes.
func PublicKeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])[:16]
}

// Keyring maps key_id to verification key for Verify.
type Keyring map[string]ed25519.PublicKey

// NewKeyring builds a Keyring from public keys, indexing each by its
// derived key_id.
func NewKeyring(pubs ...ed25519.PublicKey) Keyring {
	kr := make(Keyring, len(pubs))
	for _, pub := range pubs {
		kr[PublicKeyID(pub)] = pub
	}
	return kr
}

// LoadPublicKey reads a verification key from a file holding the 32-byte
// public key as 64 lowercase hex characters.
func LoadPublicKey(path string) (ed25519.PublicKey, error) {
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("read public key file: %w", err)
	}
	key, err := decodeKeyHex(raw, ed25519.PublicKeySize)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return ed25519.PublicKey(key), nil
}

// GenerateKeyPair creates a new ed25519 signing key pair and writes the
// seed to privPath (mode 0600) and the public key to pubPath (mode 0644),
// both as lowercase hex. It refuses to overwrite existing files: replacing
// a signing key in place would violate the rotation rule of
// evidence-schema.md §6 (rotation adds keys, never replaces them). Returns
// the derived key_id.
func GenerateKeyPair(privPath, pubPath string) (string, error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return "", fmt.Errorf("generate seed: %w", err)
	}
	if err := writeExclusive(privPath, hex.EncodeToString(seed)+"\n", 0o600); err != nil {
		return "", err
	}
	signer := NewSignerFromSeed(seed)
	if err := writeExclusive(pubPath, hex.EncodeToString(signer.PublicKey())+"\n", 0o644); err != nil {
		// The private key was created by this call; removing it keeps a
		// failed generation atomic instead of leaving a half pair behind.
		return "", errors.Join(err, os.Remove(privPath))
	}
	return signer.KeyID(), nil
}

// writeExclusive creates a file that must not exist yet and removes it
// again if the write cannot be completed. Paths are operator-supplied CLI
// inputs, cleaned before use.
func writeExclusive(path, content string, mode os.FileMode) error {
	f, err := os.OpenFile(filepath.Clean(path), os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	_, werr := f.WriteString(content)
	if werr == nil {
		// A key that reached no platter is a log nobody can verify and a
		// signer nobody can rotate away from: the records referencing it
		// would already exist.
		werr = f.Sync()
	}
	cerr := f.Close()
	if werr != nil {
		return errors.Join(fmt.Errorf("write %s: %w", path, werr), cerr, os.Remove(path))
	}
	if cerr != nil {
		return errors.Join(fmt.Errorf("close %s: %w", path, cerr), os.Remove(path))
	}
	// The bytes are durable; the name pointing at them may not be. See
	// syncDir — for a key, losing that name loses the ability to verify
	// records that already name its id.
	if serr := syncDir(path, "key"); serr != nil {
		return errors.Join(serr, os.Remove(path))
	}
	return nil
}

func decodeKeyHex(raw []byte, wantLen int) ([]byte, error) {
	s := strings.TrimSuffix(string(raw), "\n")
	if s != strings.ToLower(s) {
		return nil, fmt.Errorf("%w: hex must be lowercase", ErrKeyFormat)
	}
	key, err := hex.DecodeString(s)
	if err != nil || len(key) != wantLen {
		return nil, fmt.Errorf("%w: want %d lowercase hex-encoded bytes", ErrKeyFormat, wantLen)
	}
	return key, nil
}
