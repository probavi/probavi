package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/probavi/probavi/internal/cli"
	"github.com/probavi/probavi/internal/i18n"
	"github.com/probavi/probavi/internal/push"
)

// push.go implements `probavi push` (docs/evidence-push.md): send one
// evidence log to one URL. Exit codes are the §8 contract — a delivery
// failure (exitPushFailed) is distinct from a usage or configuration
// error, because an unreachable receiver says nothing about what a drill
// proved.
const exitPushFailed = cli.ExitError

// pushOutput is the machine-readable push result printed on stdout. The
// destination is deliberately absent: it may be a credential.
type pushOutput struct {
	Status int    `json:"status"`
	Bytes  int    `json:"bytes"`
	Path   string `json:"path"`
}

// pushFlags is the parsed command line. tokenEnvGiven distinguishes the
// default token variable from one the operator named, which is what makes
// --allow-unauthenticated and --token-env mutually exclusive detectable.
type pushFlags struct {
	log           string
	to            string
	toEnv         string
	path          string
	tokenEnv      string
	secretEnv     string
	anon          bool
	tokenEnvGiven bool
}

func runPush(args []string, stdout, stderr io.Writer, tr *i18n.T) int {
	flags, err := parsePushFlags(args, stderr)
	if err != nil {
		return exitUsage
	}
	opts, ok := resolvePush(flags, stderr, tr)
	if !ok {
		return exitUsage
	}
	client, err := push.New(opts)
	if err != nil {
		fmt.Fprintf(stderr, "probavi push: %v\n", err)
		return exitUsage
	}
	body, err := push.ReadLog(flags.log)
	if err != nil {
		fmt.Fprintf(stderr, "probavi push: %v\n", err)
		return exitUsage
	}
	if len(body) == 0 {
		// The same distinction `evidence verify` draws for an intact but
		// empty log: this exits 0 exactly as a log of proven drills does.
		tr.Fprintf(stderr, msgPushEmptyLog)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, push.Budget)
	defer cancel()

	res, err := client.Push(ctx, body)
	if err != nil {
		// A receiver's refusal names its reason here — out of licence, log
		// too large, path not accepted — because this line is what an
		// operator reads in a cron job's mail.
		fmt.Fprintf(stderr, "probavi push: %v\n", err)
		return exitPushFailed
	}
	if err := json.NewEncoder(stdout).Encode(pushOutput{
		Status: res.Status, Bytes: res.Bytes, Path: opts.Path,
	}); err != nil {
		tr.Fprintf(stderr, msgPushEncodeResult, err)
		return exitUsage
	}
	return exitPass
}

func parsePushFlags(args []string, stderr io.Writer) (*pushFlags, error) {
	fs := flag.NewFlagSet("push", flag.ContinueOnError)
	fs.SetOutput(stderr)
	p := &pushFlags{}
	fs.StringVar(&p.log, "log", "", "path to the evidence log to send, opened read-only (required)")
	fs.StringVar(&p.to, "to", "", "destination as a literal absolute http(s) URL")
	fs.StringVar(&p.toEnv, "to-env", "", "environment variable holding the destination URL")
	fs.StringVar(&p.path, "path", "", "path the log is sent under, appended to the destination URL (default: the log file's base name)")
	fs.StringVar(&p.tokenEnv, "token-env", push.DefaultTokenEnv, "environment variable holding the bearer token")
	fs.BoolVar(&p.anon, "allow-unauthenticated", false, "send no Authorization header")
	fs.StringVar(&p.secretEnv, "secret-env", "", "environment variable holding the HMAC secret for body signing")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "token-env" {
			p.tokenEnvGiven = true
		}
	})
	return p, nil
}

// resolvePush turns the command line into push options, reading every
// environment variable before the log is opened and before anything is
// sent: a missing variable must be a usage error, not a discovery made
// halfway through a delivery. Diagnostics name the variable, never its
// value.
func resolvePush(p *pushFlags, stderr io.Writer, tr *i18n.T) (push.Options, bool) {
	switch {
	case p.log == "":
		tr.Fprintf(stderr, msgPushLogRequired)
		return push.Options{}, false
	case p.to != "" && p.toEnv != "":
		tr.Fprintf(stderr, msgPushDestinationNotBoth)
		return push.Options{}, false
	case p.to == "" && p.toEnv == "":
		tr.Fprintf(stderr, msgPushDestinationNeither)
		return push.Options{}, false
	case p.anon && p.tokenEnvGiven:
		tr.Fprintf(stderr, msgPushTokenAndAnon)
		return push.Options{}, false
	}
	opts := push.Options{URL: p.to, Version: version}
	if p.toEnv != "" {
		v, ok := envValue(p.toEnv, stderr, tr)
		if !ok {
			return push.Options{}, false
		}
		opts.URL = v
	}
	if !p.anon {
		v, ok := envValue(p.tokenEnv, stderr, tr)
		if !ok {
			return push.Options{}, false
		}
		opts.Token = v
	}
	if p.secretEnv != "" {
		v, ok := envValue(p.secretEnv, stderr, tr)
		if !ok {
			return push.Options{}, false
		}
		opts.Secret = []byte(v)
	}
	path, ok := destinationPath(p, stderr, tr)
	if !ok {
		return push.Options{}, false
	}
	opts.Path = path
	return opts, true
}

// destinationPath resolves --path, defaulting to the log's base name. The
// two diagnostics differ because the mistakes differ: an explicit path is
// corrected where it was typed, while a rejected default has to point at
// the flag that overrides it.
func destinationPath(p *pushFlags, stderr io.Writer, tr *i18n.T) (string, bool) {
	if p.path != "" {
		if err := push.ValidatePath(p.path); err != nil {
			tr.Fprintf(stderr, msgPushPathInvalid, p.path, err)
			return "", false
		}
		return p.path, true
	}
	derived := push.DefaultPath(p.log)
	if err := push.ValidatePath(derived); err != nil {
		tr.Fprintf(stderr, msgPushPathDerived, derived, err)
		return "", false
	}
	return derived, true
}

func envValue(name string, stderr io.Writer, tr *i18n.T) (string, bool) {
	v := os.Getenv(name)
	if v == "" {
		tr.Fprintf(stderr, msgPushEnvUnset, name)
		return "", false
	}
	return v, true
}
