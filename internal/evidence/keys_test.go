package evidence

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// testSeed is a fixed 32-byte seed: deterministic signatures keep the
// golden files stable.
func testSeed() []byte {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i)
	}
	return seed
}

func testSigner() *Signer { return NewSignerFromSeed(testSeed()) }

func writeKeyFile(t *testing.T, name string, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	return path
}

func TestLoadSigner(t *testing.T) {
	seedHex := hex.EncodeToString(testSeed()) + "\n"

	s, err := LoadSigner(writeKeyFile(t, "ed25519.key", seedHex, 0o600))
	if err != nil {
		t.Fatalf("LoadSigner: %v", err)
	}
	if s.KeyID() != testSigner().KeyID() {
		t.Errorf("KeyID = %q, want %q", s.KeyID(), testSigner().KeyID())
	}

	if _, err := LoadSigner(writeKeyFile(t, "open.key", seedHex, 0o644)); !errors.Is(err, ErrKeyPermissions) {
		t.Errorf("world-readable key: got %v, want ErrKeyPermissions", err)
	}
	if _, err := LoadSigner(writeKeyFile(t, "short.key", "abcd\n", 0o600)); !errors.Is(err, ErrKeyFormat) {
		t.Errorf("short key: got %v, want ErrKeyFormat", err)
	}
	if _, err := LoadSigner(writeKeyFile(t, "upper.key", "AB"+seedHex[2:], 0o600)); !errors.Is(err, ErrKeyFormat) {
		t.Errorf("uppercase key: got %v, want ErrKeyFormat", err)
	}
	if _, err := LoadSigner(filepath.Join(t.TempDir(), "missing.key")); err == nil {
		t.Error("missing key file: expected error")
	}
}

func TestLoadPublicKey(t *testing.T) {
	pubHex := hex.EncodeToString(testSigner().PublicKey()) + "\n"
	pub, err := LoadPublicKey(writeKeyFile(t, "ed25519.pub", pubHex, 0o644))
	if err != nil {
		t.Fatalf("LoadPublicKey: %v", err)
	}
	if PublicKeyID(pub) != testSigner().KeyID() {
		t.Errorf("PublicKeyID = %q, want %q", PublicKeyID(pub), testSigner().KeyID())
	}
	if _, err := LoadPublicKey(writeKeyFile(t, "bad.pub", "zz\n", 0o644)); !errors.Is(err, ErrKeyFormat) {
		t.Errorf("bad public key: got %v, want ErrKeyFormat", err)
	}
}

func TestPublicKeyIDShape(t *testing.T) {
	id := PublicKeyID(testSigner().PublicKey())
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(id) {
		t.Errorf("PublicKeyID = %q, want 16 lowercase hex chars", id)
	}
}

func TestGenerateKeyPair(t *testing.T) {
	dir := t.TempDir()
	priv := filepath.Join(dir, "ed25519.key")
	pub := priv + ".pub"

	keyID, err := GenerateKeyPair(priv, pub)
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	signer, err := LoadSigner(priv)
	if err != nil {
		t.Fatalf("LoadSigner on generated key: %v", err)
	}
	if signer.KeyID() != keyID {
		t.Errorf("KeyID = %q, want %q", signer.KeyID(), keyID)
	}
	loaded, err := LoadPublicKey(pub)
	if err != nil {
		t.Fatalf("LoadPublicKey on generated key: %v", err)
	}
	if PublicKeyID(loaded) != keyID {
		t.Errorf("public key derives %q, want %q", PublicKeyID(loaded), keyID)
	}
	if info, err := os.Stat(pub); err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("public key mode/err = %v/%v, want 0644", info.Mode().Perm(), err)
	}

	if _, err := GenerateKeyPair(priv, pub); err == nil {
		t.Error("GenerateKeyPair overwrote an existing key")
	}
}

func TestGenerateKeyPairAtomicity(t *testing.T) {
	dir := t.TempDir()
	priv := filepath.Join(dir, "ed25519.key")
	// Public key path in a nonexistent directory: generation must fail AND
	// must not leave the freshly created private key behind.
	pub := filepath.Join(dir, "no", "such", "dir", "ed25519.pub")

	if _, err := GenerateKeyPair(priv, pub); err == nil {
		t.Fatal("GenerateKeyPair: expected error for uncreatable public key path")
	}
	if _, err := os.Stat(priv); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("private key left behind after failed generation: stat err = %v", err)
	}

	if _, err := GenerateKeyPair(filepath.Join(dir, "no", "such", "k"), pub); err == nil {
		t.Error("GenerateKeyPair: expected error for uncreatable private key path")
	}
}

func TestKeyring(t *testing.T) {
	kr := NewKeyring(testSigner().PublicKey())
	if _, ok := kr[testSigner().KeyID()]; !ok {
		t.Error("keyring does not resolve the signer's key_id")
	}
	if len(kr) != 1 {
		t.Errorf("keyring size = %d, want 1", len(kr))
	}
}

// TestLoadSignerRefusesWhatIsNotAKeyFile covers the paths a plain read
// would obey instead of refusing.
//
// Every case here is reachable by a typo in one config field, and the
// field names the key that signs every record — so the answer has to be
// a refusal that says what is wrong, not a drill that never reports.
func TestLoadSignerRefusesWhatIsNotAKeyFile(t *testing.T) {
	dir := t.TempDir()

	t.Run("a fifo", func(t *testing.T) {
		// A plain read waits here for a writer that never comes. The
		// non-blocking open is what turns that hang into an answer, so
		// this test would not finish without it.
		path := filepath.Join(dir, "fifo.key")
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Skipf("cannot create a fifo here: %v", err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := LoadSigner(path)
			done <- err
		}()
		select {
		case err := <-done:
			if !errors.Is(err, ErrKeyFormat) {
				t.Errorf("fifo: got %v, want ErrKeyFormat", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("LoadSigner blocked on a fifo — the open must not wait for a writer")
		}
	})

	t.Run("a character device", func(t *testing.T) {
		// /dev/zero answers a read forever: without the mode check this
		// reads until the host runs out of memory.
		if _, err := os.Stat("/dev/zero"); err != nil {
			t.Skipf("no /dev/zero here: %v", err)
		}
		if _, err := LoadSigner("/dev/zero"); !errors.Is(err, ErrKeyFormat) {
			t.Errorf("/dev/zero: got %v, want ErrKeyFormat", err)
		}
	})

	t.Run("a directory", func(t *testing.T) {
		if _, err := LoadSigner(dir); err == nil {
			t.Error("a directory is not a key file")
		}
	})

	t.Run("a missing file", func(t *testing.T) {
		if _, err := LoadSigner(filepath.Join(dir, "absent.key")); err == nil {
			t.Error("a missing key file must be reported")
		}
	})
}

// TestLoadSignerBoundsWhatItReads keeps an oversized file from being read
// into memory whole. It still fails as a format error, because that is
// what is actually wrong with it.
func TestLoadSignerBoundsWhatItReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "huge.key")
	if err := os.WriteFile(path, bytes.Repeat([]byte("a"), 8<<20), 0o600); err != nil {
		t.Fatalf("write huge key: %v", err)
	}
	if _, err := LoadSigner(path); !errors.Is(err, ErrKeyFormat) {
		t.Errorf("oversized key: got %v, want ErrKeyFormat", err)
	}
}

// TestGenerateKeyPairReportsAnUnsyncableDirectory covers the durability
// step's own failure. A key whose bytes reached the disk under a name
// that did not is a key nobody can verify records against, so the pair is
// removed and the failure reported rather than half-kept.
func TestGenerateKeyPairReportsAnUnsyncableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permissions do not deny anything")
	}
	// Write and traverse but not read: a file can be created here, and
	// the directory itself cannot be opened to sync it.
	dir := filepath.Join(t.TempDir(), "closed")
	if err := os.Mkdir(dir, 0o300); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) }) //nolint:errcheck // best effort, so t.TempDir can clean up

	_, err := GenerateKeyPair(filepath.Join(dir, "ed25519.key"), filepath.Join(dir, "ed25519.pub"))
	if err == nil {
		t.Fatal("GenerateKeyPair reported success for a key it could not make durable")
	}
	if !strings.Contains(err.Error(), "key directory") {
		t.Errorf("error = %q, want it to name the durability step that failed", err)
	}
}
