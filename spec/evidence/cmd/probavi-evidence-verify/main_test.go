package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// examplesDir is the published worked example, four levels up from this
// command's package directory.
const examplesDir = "../../../../docs/schemas/evidence/examples"

func examplePath(name string) string { return filepath.Join(examplesDir, name) }

// runCLI drives the command exactly as main does and returns its exit code
// together with both streams.
func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// copyExample writes a mutated copy of an example log into a temp file and
// returns its path.
func copyExample(t *testing.T, name string, mutate func([]byte) []byte) string {
	t.Helper()
	data, err := os.ReadFile(examplePath(name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if mutate != nil {
		data = mutate(data)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// The exit codes below are the §9 contract this command exists to expose.
// A cron job or CI step branches on them, which makes them as much a public
// interface as the log format itself.

func TestValidLogExitsZero(t *testing.T) {
	code, stdout, stderr := runCLI(t, "--log", examplePath("log_v1.jsonl"), "--key", examplePath("signer.pub"))
	if code != exitValid {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitValid, stderr)
	}
	var res struct {
		Status  string `json:"status"`
		Records int    `json:"records"`
	}
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("stdout is not JSON (%q): %v", stdout, err)
	}
	if res.Status != "VALID" || res.Records != 3 {
		t.Errorf("result = %s with %d records, want VALID with 3", res.Status, res.Records)
	}
}

// TestEmptyLogWarnsWithoutChangingTheVerdict covers the verdict most likely
// to be misread: an intact log with nothing in it verifies, and exits 0
// exactly as a log of verified drills does. The §9 exit code stays; the
// difference is said out loud on stderr.
func TestEmptyLogWarnsWithoutChangingTheVerdict(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatalf("write empty log: %v", err)
	}
	code, stdout, stderr := runCLI(t, "--log", empty, "--key", examplePath("signer.pub"))
	if code != exitValid {
		t.Fatalf("exit = %d, want %d — the §9 exit code must not move", code, exitValid)
	}
	var res struct {
		Status  string `json:"status"`
		Records int    `json:"records"`
	}
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("stdout is not JSON (%q): %v", stdout, err)
	}
	if res.Status != "VALID" || res.Records != 0 {
		t.Errorf("result = %s with %d records, want VALID with 0", res.Status, res.Records)
	}
	if !strings.Contains(stderr, "holds no records") {
		t.Errorf("stderr = %q, want it to say the log proves nothing", stderr)
	}
}

func TestEveryPublishedVersionVerifies(t *testing.T) {
	key := examplePath("signer.pub")
	for _, name := range []string{"log_v0.jsonl", "log_v1.jsonl", "log_v2.jsonl"} {
		if code, _, stderr := runCLI(t, "--log", examplePath(name), "--key", key); code != exitValid {
			t.Errorf("%s: exit = %d, want 0 (stderr: %s)", name, code, stderr)
		}
	}
}

func TestTamperedLogExitsTwo(t *testing.T) {
	path := copyExample(t, "log_v1.jsonl", func(b []byte) []byte {
		return bytes.Replace(b, []byte(`"outcome":"pass"`), []byte(`"outcome":"fail"`), 1)
	})
	code, stdout, _ := runCLI(t, "--log", path, "--key", examplePath("signer.pub"))
	if code != exitInvalid {
		t.Fatalf("exit = %d, want %d", code, exitInvalid)
	}
	if !strings.Contains(stdout, "INVALID") {
		t.Errorf("stdout = %q, want an INVALID result", stdout)
	}
}

func TestTornTailExitsOne(t *testing.T) {
	path := copyExample(t, "log_v1.jsonl", func(b []byte) []byte {
		return append(b, []byte(`{"schema":"probavi-ev`)...)
	})
	code, stdout, _ := runCLI(t, "--log", path, "--key", examplePath("signer.pub"))
	if code != exitValidDamage {
		t.Fatalf("exit = %d, want %d", code, exitValidDamage)
	}
	if !strings.Contains(stdout, "VALID_WITH_DAMAGE") {
		t.Errorf("stdout = %q, want VALID_WITH_DAMAGE", stdout)
	}
}

func TestUsageErrorsExitThree(t *testing.T) {
	key := examplePath("signer.pub")
	cases := []struct {
		name string
		args []string
	}{
		{"no arguments", nil},
		{"log without key", []string{"--log", examplePath("log_v1.jsonl")}},
		{"key without log", []string{"--key", key}},
		{"unknown flag", []string{"--nope"}},
		{"missing log file", []string{"--log", "does-not-exist.jsonl", "--key", key}},
		{"missing key file", []string{"--log", examplePath("log_v1.jsonl"), "--key", "does-not-exist.pub"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code, _, _ := runCLI(t, tc.args...); code != exitUsage {
				t.Errorf("exit = %d, want %d", code, exitUsage)
			}
		})
	}
}

// TestMalformedKeyFileIsUsageError keeps a bad key file from being reported
// as a bad log: the difference matters when the answer is going to an
// auditor.
func TestMalformedKeyFileIsUsageError(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.pub")
	if err := os.WriteFile(bad, []byte("not a key"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	code, _, stderr := runCLI(t, "--log", examplePath("log_v1.jsonl"), "--key", bad)
	if code != exitUsage {
		t.Fatalf("exit = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "hex characters") {
		t.Errorf("stderr = %q, want it to explain the key format", stderr)
	}
}

// TestUnwritableStdoutIsReported covers the last failure the command can
// meet: it computed a verdict but could not deliver it. Reporting VALID down
// a broken pipe would be the worst possible lie, so this must exit 3.
func TestUnwritableStdoutIsReported(t *testing.T) {
	var stderr bytes.Buffer
	code := run(
		[]string{"--log", examplePath("log_v1.jsonl"), "--key", examplePath("signer.pub")},
		failingWriter{},
		&stderr,
	)
	if code != exitUsage {
		t.Fatalf("exit = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr.String(), "encode result") {
		t.Errorf("stderr = %q, want it to name the encoding failure", stderr.String())
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("pipe closed") }

// TestMultipleKeysAccepted covers the rotation case of §6: a verifier may be
// handed several public keys and must select by key_id.
func TestMultipleKeysAccepted(t *testing.T) {
	other := filepath.Join(t.TempDir(), "other.pub")
	if err := os.WriteFile(other, []byte(strings.Repeat("ab", 32)+"\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	code, _, stderr := runCLI(t,
		"--log", examplePath("log_v1.jsonl"),
		"--key", other,
		"--key", examplePath("signer.pub"),
	)
	if code != exitValid {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
}
