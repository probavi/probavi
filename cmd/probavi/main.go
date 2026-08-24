// Package main is the probavi CLI entry point. It contains flag parsing,
// wiring, and exit codes only — all logic lives in internal packages
// (AGENTS.md layout rule).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/probavi/probavi/internal/adapter"
	"github.com/probavi/probavi/internal/cli"
	"github.com/probavi/probavi/internal/evidence"
	"github.com/probavi/probavi/internal/i18n"
)

// Exit codes. Verify verdicts follow evidence-schema.md §9; exitUsage
// covers usage and I/O errors, distinct from any verdict. The numbers come
// from internal/cli, which declares the contract the capabilities manifest
// publishes; these names are this file's reading of them.
const (
	exitValid           = cli.ExitPass
	exitValidWithDamage = cli.ExitFail
	exitInvalid         = cli.ExitError
	exitUsage           = cli.ExitUsage
)

func main() {
	tr, err := i18n.New(i18n.Detect(os.Getenv))
	if err != nil {
		// A broken embedded catalog is a build defect; fall back to the
		// canonical English loudly, never crash a cron drill over prose.
		fmt.Fprintf(os.Stderr, "probavi: %v\n", err)
		tr = i18n.English()
	}
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, tr))
}

// handler runs one resolved command.
type handler func(args []string, stdout, stderr io.Writer, tr *i18n.T) int

// handlers wires every command internal/cli declares to its
// implementation. The table there is the CLI contract — a command absent
// from it cannot be invoked, and a command present in it without a handler
// here is a build defect, both pinned by the cmd test suite.
func handlers() map[string]handler {
	return map[string]handler{
		cli.CmdRun:                runDrill,
		cli.CmdGameDay:            runGameDay,
		cli.CmdPush:               runPush,
		cli.CmdEvidenceVerify:     runEvidenceVerify,
		cli.CmdEvidenceKeygen:     runEvidenceKeygen,
		cli.CmdAdapterProbe:       runAdapterProbe,
		cli.CmdAdapterConformance: runAdapterConformance,
		cli.CmdVersion: func(_ []string, stdout, _ io.Writer, tr *i18n.T) int {
			return runVersion(stdout, tr)
		},
	}
}

// groupMessages maps a command group to its unknown-subcommand
// diagnostic; the cmd test suite pins it to cli.Groups().
func groupMessages() map[string]string {
	return map[string]string{
		"adapter":  msgUnknownAdapterSub,
		"evidence": msgUnknownEvidenceSub,
	}
}

func run(args []string, stdout, stderr io.Writer, tr *i18n.T) int {
	if len(args) == 0 {
		usage(stderr, tr)
		return exitUsage
	}
	m := cli.Resolve(args)
	switch m.Resolution {
	case cli.ResolvedCommand:
		return dispatch(handlers(), m, stdout, stderr, tr)
	case cli.UnknownCommand:
		tr.Fprintf(stderr, msgUnknownCommand, m.Word)
	case cli.UnknownSubcommand:
		format, ok := groupMessages()[m.Group]
		if !ok {
			format = msgUnknownCommand
		}
		tr.Fprintf(stderr, format, m.Word)
	case cli.IncompleteGroup:
		// A bare group word is answered by the usage text alone, which
		// lists every subcommand.
	}
	usage(stderr, tr)
	return exitUsage
}

// dispatch invokes the resolved command. The handler table is a parameter
// so the missing-handler path — a build defect, never a user error — is
// reachable from tests.
func dispatch(table map[string]handler, m cli.Match, stdout, stderr io.Writer, tr *i18n.T) int {
	h, ok := table[m.Command.ID]
	if !ok {
		tr.Fprintf(stderr, msgUnknownCommand, m.Command.ID)
		usage(stderr, tr)
		return exitUsage
	}
	return h(m.Args, stdout, stderr, tr)
}

// verifyOutput is the machine-readable verify result printed on stdout.
type verifyOutput struct {
	Status       string `json:"status"`
	Records      int    `json:"records"`
	DamagedLines []int  `json:"damaged_lines"`
	FailedLine   int    `json:"failed_line"`
	Reason       string `json:"reason"`
}

func runEvidenceVerify(args []string, stdout, stderr io.Writer, tr *i18n.T) int {
	fs := flag.NewFlagSet("evidence verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	logPath := fs.String("log", "", "path to the evidence log file (required)")
	var keyPaths stringList
	fs.Var(&keyPaths, "key", "public key file; repeat to build a keyring (required)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *logPath == "" || len(keyPaths) == 0 {
		tr.Fprintf(stderr, msgVerifyFlagsRequired)
		return exitUsage
	}

	keyring, err := loadKeyring(keyPaths)
	if err != nil {
		fmt.Fprintf(stderr, "probavi evidence verify: %v\n", err)
		return exitUsage
	}
	f, err := os.Open(*logPath)
	if err != nil {
		fmt.Fprintf(stderr, "probavi evidence verify: %v\n", err)
		return exitUsage
	}
	defer closeQuietly(f, stderr)

	res, err := evidence.Verify(f, keyring)
	if err != nil {
		fmt.Fprintf(stderr, "probavi evidence verify: %v\n", err)
		return exitUsage
	}
	out := verifyOutput{
		Status:       res.Status.String(),
		Records:      res.Records,
		DamagedLines: res.DamagedLines,
		FailedLine:   res.FailedLine,
		Reason:       res.Reason,
	}
	if out.DamagedLines == nil {
		out.DamagedLines = []int{}
	}
	if err := json.NewEncoder(stdout).Encode(out); err != nil {
		tr.Fprintf(stderr, msgVerifyEncodeResult, err)
		return exitUsage
	}
	// A log that is intact and empty is VALID, and that is the right answer
	// to the question the verifier asks — whether the file was tampered
	// with, not whether drills were run. Both answers exit 0, so the one
	// that proves nothing says so.
	if res.Status == evidence.StatusValid && res.Records == 0 {
		tr.Fprintf(stderr, msgVerifyNoRecords)
	}
	switch res.Status {
	case evidence.StatusValid:
		return exitValid
	case evidence.StatusValidWithDamage:
		return exitValidWithDamage
	default:
		return exitInvalid
	}
}

// keygenOutput is the machine-readable keygen result printed on stdout.
// It never contains key material.
type keygenOutput struct {
	KeyID         string `json:"key_id"`
	KeyFile       string `json:"key_file"`
	PublicKeyFile string `json:"public_key_file"`
}

func runEvidenceKeygen(args []string, stdout, stderr io.Writer, tr *i18n.T) int {
	fs := flag.NewFlagSet("evidence keygen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "", "path for the private key; the public key is written next to it as <path>.pub (required)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *out == "" {
		tr.Fprintf(stderr, msgKeygenOutRequired)
		return exitUsage
	}
	pubPath := *out + ".pub"
	keyID, err := evidence.GenerateKeyPair(*out, pubPath)
	if err != nil {
		fmt.Fprintf(stderr, "probavi evidence keygen: %v\n", err)
		return exitUsage
	}
	if err := json.NewEncoder(stdout).Encode(keygenOutput{KeyID: keyID, KeyFile: *out, PublicKeyFile: pubPath}); err != nil {
		tr.Fprintf(stderr, msgKeygenEncodeResult, err)
		return exitUsage
	}
	return exitValid
}

func loadKeyring(paths []string) (evidence.Keyring, error) {
	keyring := evidence.Keyring{}
	for _, p := range paths {
		pub, err := evidence.LoadPublicKey(p)
		if err != nil {
			return nil, err
		}
		keyring[evidence.PublicKeyID(pub)] = pub
	}
	return keyring, nil
}

func closeQuietly(c io.Closer, stderr io.Writer) {
	if err := c.Close(); err != nil {
		fmt.Fprintf(stderr, "probavi: close: %v\n", err)
	}
}

// stringList collects a repeatable string flag.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// runVersion prints the binary version and the contract versions this
// build speaks. For a trust product the contracts matter as much as the
// binary: an auditor's first question about a log is "written under which
// schema", and an adapter author's is "against which protocol".
func runVersion(stdout io.Writer, tr *i18n.T) int {
	fmt.Fprintf(stdout, "probavi %s %s/%s\n", version, runtime.GOOS, runtime.GOARCH)
	tr.Fprintf(stdout, msgVersionProtocol, adapter.ProtocolVersion)
	tr.Fprintf(stdout, msgVersionSchema, evidence.SchemaID)
	return 0
}

func usage(w io.Writer, tr *i18n.T) {
	tr.Fprintf(w, msgUsage)
}
