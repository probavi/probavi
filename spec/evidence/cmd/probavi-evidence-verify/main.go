// Command probavi-evidence-verify verifies a Probavi evidence log offline
// against one or more ed25519 public keys.
//
// It is an independent implementation of docs/evidence-schema.md §9, sharing
// no code with the Probavi core. Anyone holding a log file and a public key
// can check a recoverability claim without trusting — or even installing —
// the tool that produced it.
//
// Usage:
//
//	probavi-evidence-verify --log <file> --key <pubkey> [--key <pubkey> ...]
//
// Exit codes follow §9: 0 VALID, 1 VALID_WITH_DAMAGE, 2 INVALID, 3 usage or
// I/O error.
package main

import (
	"crypto/ed25519"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	evidence "github.com/probavi/probavi/spec/evidence"
)

// keyList collects repeated --key flags.
type keyList []string

func (k *keyList) String() string { return fmt.Sprint(*k) }

func (k *keyList) Set(v string) error {
	*k = append(*k, v)
	return nil
}

const (
	exitValid       = 0
	exitValidDamage = 1
	exitInvalid     = 2
	exitUsage       = 3
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("probavi-evidence-verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	logPath := fs.String("log", "", "path to the evidence log (JSONL)")
	var keyPaths keyList
	fs.Var(&keyPaths, "key", "path to an ed25519 public key file (repeatable)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *logPath == "" || len(keyPaths) == 0 {
		fmt.Fprintln(stderr, "probavi-evidence-verify: --log and at least one --key are required")
		fs.Usage()
		return exitUsage
	}

	keys := make([]ed25519.PublicKey, 0, len(keyPaths))
	for _, p := range keyPaths {
		data, err := os.ReadFile(p)
		if err != nil {
			fmt.Fprintf(stderr, "probavi-evidence-verify: read key %s: %v\n", p, err)
			return exitUsage
		}
		pub, err := evidence.ParsePublicKey(data)
		if err != nil {
			fmt.Fprintf(stderr, "probavi-evidence-verify: %s: %v\n", p, err)
			return exitUsage
		}
		keys = append(keys, pub)
	}

	f, err := os.Open(*logPath)
	if err != nil {
		fmt.Fprintf(stderr, "probavi-evidence-verify: %v\n", err)
		return exitUsage
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			fmt.Fprintf(stderr, "probavi-evidence-verify: close: %v\n", cerr)
		}
	}()

	res, err := evidence.Verify(f, evidence.NewKeyring(keys...))
	if err != nil {
		fmt.Fprintf(stderr, "probavi-evidence-verify: %v\n", err)
		return exitUsage
	}

	enc := json.NewEncoder(stdout)
	if err := enc.Encode(res); err != nil {
		fmt.Fprintf(stderr, "probavi-evidence-verify: encode result: %v\n", err)
		return exitUsage
	}

	// An intact, empty log is VALID — the right answer to the question this
	// tool asks, which is whether the file was tampered with rather than
	// whether drills were run. It also exits 0, exactly like a log of
	// verified drills, so the case that proves nothing says so. The exit
	// code is normative (evidence-schema.md §9) and does not move.
	if res.Status == evidence.StatusValid && res.Records == 0 {
		fmt.Fprintln(stderr, "probavi-evidence-verify: the log is intact but holds no records — "+
			"nothing has been proven, and this exits 0 exactly as a log of verified drills does")
	}

	switch res.Status {
	case evidence.StatusValid:
		return exitValid
	case evidence.StatusValidDamage:
		return exitValidDamage
	default:
		return exitInvalid
	}
}
