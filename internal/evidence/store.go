package evidence

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
)

// Store is the append-only, single-writer evidence log writer
// (evidence-schema.md §2). It never rewrites existing bytes: the only
// mutation it ever performs is appending.
type Store struct {
	f      *os.File
	lock   *os.File
	signer *Signer
	logger *slog.Logger

	nextSeq  int64
	prevHash string
	broken   bool
}

// Open locks and opens an evidence log for appending, creating it if
// needed. It validates the existing chain (without signature checks — the
// log may contain records signed by rotated keys), closes a torn tail by
// appending a newline, and resumes the chain from the last valid record.
func Open(path string, signer *Signer, logger *slog.Logger) (*Store, error) {
	if signer == nil {
		return nil, errors.New("open evidence store: signer is required")
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	lock, err := acquireLock(path + ".lock")
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Clean(path), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("open evidence log: %w", err), lock.Close())
	}
	if err := syncDir(path, "evidence log"); err != nil {
		return nil, errors.Join(err, f.Close(), lock.Close())
	}
	st := &Store{f: f, lock: lock, signer: signer, logger: logger}
	if err := st.resume(); err != nil {
		return nil, errors.Join(err, f.Close(), lock.Close())
	}
	return st, nil
}

// Append fills the chain fields of rec, signs it, and appends its canonical
// line. rec must arrive with Seq, PrevHash, and Sig unset. On success rec
// carries the stored values.
func (s *Store) Append(rec *Record) error {
	if s.broken {
		return errors.New("append: store is in a failed state, reopen to resume")
	}
	if rec.Sig != nil || rec.Seq != 0 || rec.PrevHash != "" {
		return fmt.Errorf("%w: seq, prev_hash and sig are set by the store", ErrInvalidRecord)
	}
	rec.Seq = s.nextSeq
	rec.PrevHash = s.prevHash
	line, err := s.sealed(rec)
	if err != nil {
		rec.Seq, rec.PrevHash, rec.Sig = 0, "", nil
		return err
	}
	if err := s.write(line); err != nil {
		return err
	}
	s.prevHash = lineHash(line)
	s.nextSeq++
	return nil
}

// NextSeq returns the sequence number the next appended record will get.
func (s *Store) NextSeq() int64 { return s.nextSeq }

// Close releases the log and its writer lock.
func (s *Store) Close() error {
	err := s.f.Close()
	if lerr := s.lock.Close(); lerr != nil && err == nil {
		err = lerr
	}
	if err != nil {
		return fmt.Errorf("close evidence store: %w", err)
	}
	return nil
}

// sealed validates rec, signs its canonical bytes, and returns the full
// canonical stored line.
func (s *Store) sealed(rec *Record) ([]byte, error) {
	if err := rec.Validate(); err != nil {
		return nil, err
	}
	message, err := CanonicalizeRecord(rec)
	if err != nil {
		return nil, err
	}
	rec.Sig = &Signature{
		Alg:    "ed25519",
		KeyID:  s.signer.KeyID(),
		SigB64: base64.StdEncoding.EncodeToString(s.signer.sign(message)),
	}
	line, err := CanonicalizeRecord(rec)
	if err != nil {
		return nil, err
	}
	if len(line) > MaxRecordBytes {
		return nil, fmt.Errorf("%w: %d bytes", ErrRecordTooLarge, len(line))
	}
	return line, nil
}

// write appends one line plus newline and fsyncs. A failed write or sync
// poisons the store: the file tail state is unknown, so continuing to
// append could corrupt the chain; reopening re-validates and recovers.
func (s *Store) write(line []byte) error {
	if _, err := s.f.Write(append(line, '\n')); err != nil {
		s.broken = true
		return fmt.Errorf("append record: %w", err)
	}
	if err := s.f.Sync(); err != nil {
		s.broken = true
		return fmt.Errorf("fsync evidence log: %w", err)
	}
	return nil
}

// resume closes a torn tail if present, then walks the existing chain to
// find the resume point.
func (s *Store) resume() error {
	if err := s.closeTornTail(); err != nil {
		return err
	}
	if _, err := s.f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek evidence log: %w", err)
	}
	w, err := walk(s.f, nil, nil)
	if err != nil {
		return err
	}
	if w.failed {
		return fmt.Errorf("%w: line %d: %s", ErrChainState, w.failedLine, w.reason)
	}
	if len(w.damaged) > 0 {
		s.logger.Warn("evidence log contains damaged lines (crash artifacts); chain continues from last valid record",
			"path", s.f.Name(), "damaged_lines", w.damaged)
	}
	s.nextSeq = w.nextSeq
	s.prevHash = w.prevHash
	return nil
}

// closeTornTail appends a single newline if the file does not end with one
// — a pure append; existing bytes are never rewritten (schema §2).
func (s *Store) closeTornTail() error {
	info, err := s.f.Stat()
	if err != nil {
		return fmt.Errorf("stat evidence log: %w", err)
	}
	if info.Size() == 0 {
		return nil
	}
	var last [1]byte
	if _, err := s.f.ReadAt(last[:], info.Size()-1); err != nil {
		return fmt.Errorf("read evidence log tail: %w", err)
	}
	if last[0] == '\n' {
		return nil
	}
	s.logger.Warn("evidence log has a torn tail (crash mid-write); closing the fragment", "path", s.f.Name())
	if _, err := s.f.Write([]byte("\n")); err != nil {
		return fmt.Errorf("close torn tail: %w", err)
	}
	return nil
}

// syncDir flushes the directory that holds path, once, after the file
// itself is durable. what names the file for the error messages.
//
// Fsyncing a file promises its bytes reached the disk — but the name
// pointing at those bytes lives in the parent directory, and for a file
// that has just been created that entry may still be in cache. A crash
// there loses the whole file, fsynced content and all.
//
// Both callers lose something they cannot reconstruct. For an append-only
// evidence log it is the proof that a drill ran at all. For a signing key
// it is worse: the records naming that key id already exist, so the key
// cannot be rotated away from, and nobody can verify what it signed.
//
// EINVAL means the filesystem does not support syncing a directory. That
// is not a reason to refuse to run a drill, so it is the one error passed
// over; anything else is a durability guarantee this package cannot make
// and says so rather than pretending.
func syncDir(path, what string) error {
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open %s directory: %w", what, err)
	}
	serr := d.Sync()
	cerr := d.Close()
	if serr != nil && !errors.Is(serr, syscall.EINVAL) {
		return errors.Join(fmt.Errorf("sync %s directory: %w", what, serr), cerr)
	}
	if cerr != nil {
		return fmt.Errorf("close %s directory: %w", what, cerr)
	}
	return nil
}

// acquireLock takes the advisory single-writer lock next to the log file.
func acquireLock(lockPath string) (*os.File, error) {
	lock, err := os.OpenFile(filepath.Clean(lockPath), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		cerr := lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.Join(fmt.Errorf("%w: %s", ErrLocked, lockPath), cerr)
		}
		return nil, errors.Join(fmt.Errorf("flock %s: %w", lockPath, err), cerr)
	}
	return lock, nil
}
