// Command versionmatrix prints the engine versions CI has to restore from,
// as the JSON a GitHub Actions job matrix consumes.
//
// The list is derived from the adapter manifests rather than written into
// a workflow, because the manifests are what docs/capabilities.json
// publishes. A version listed there but absent from the matrix would be a
// claim nothing exercises; a version in the matrix but absent from there
// would be work nobody claims. Deriving one from the other removes the
// gap instead of asking a reviewer to notice it (docs/engine-versions.md
// §2).
//
// With -scope it answers the other question the workflow asks: given the
// paths a pull request changed, how wide does this run have to be. That
// decision lives here rather than in shell inside the workflow so it can
// be tested case by case — a narrowing that is wrong in one direction
// wastes an hour of CI, and in the other lets an untested engine merge.
//
// It is a repository tool, not a shipped binary.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/probavi/probavi/internal/capabilities"
)

// target is one matrix job: restore this adapter's fixtures inside this
// engine image. The fields are consumed by name in the workflow, so
// renaming one is a workflow change too.
type target struct {
	Adapter       string `json:"adapter"`
	EngineVersion string `json:"engine_version"`
	Image         string `json:"image"`
	Baseline      bool   `json:"baseline"`
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "versionmatrix: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("versionmatrix", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", ".", "repository root")
	baselinesOnly := fs.Bool("baselines-only", false,
		"emit only each adapter's baseline version — the everyday gate")
	adapters := fs.String("adapters", "",
		"comma-separated adapter ids to keep — the scope of an adapter-local pull request; "+
			"empty keeps every adapter, and an id no manifest declares is an error")
	scoped := fs.Bool("scope", false,
		"read a pull request's changed paths on stdin and write the workflow's scope "+
			"outputs (full=…, adapters=…) instead of a matrix")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *scoped {
		return runScope(*root, stdin, stdout)
	}

	targets, err := enumerate(*root)
	if err != nil {
		return err
	}
	if *baselinesOnly {
		targets = baselines(targets)
	}
	if *adapters != "" {
		if targets, err = filterAdapters(targets, *adapters); err != nil {
			return err
		}
	}
	out, err := json.Marshal(targets)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, string(out))
	return nil
}

// baselines keeps one target per adapter: the version every push has to
// restore from. The manifest gate guarantees exactly one exists per
// adapter, so this cannot silently drop an adapter from the run.
func baselines(targets []target) []target {
	out := make([]target, 0, len(targets))
	for _, t := range targets {
		if t.Baseline {
			out = append(out, t)
		}
	}
	return out
}

// filterAdapters keeps the targets of the named adapters, preserving
// enumeration order. Every name must match an enumerated adapter: a
// typo that silently shrank the matrix would be this tool's one failure
// mode, so an unknown or empty id is an error, checked in the order the
// caller named them.
func filterAdapters(targets []target, list string) ([]target, error) {
	names := strings.Split(list, ",")
	present := map[string]bool{}
	for _, t := range targets {
		present[t.Adapter] = true
	}
	want := map[string]bool{}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, fmt.Errorf("-adapters names an empty adapter id in %q", list)
		}
		if !present[name] {
			return nil, fmt.Errorf("-adapters names %q, which no manifest declares", name)
		}
		want[name] = true
	}
	out := make([]target, 0, len(targets))
	for _, t := range targets {
		if want[t.Adapter] {
			out = append(out, t)
		}
	}
	return out, nil
}

// scopeDecision is what a pull request's changed paths imply: the whole
// matrix, every adapter's baseline, or the baselines of the adapters the
// change actually touched.
type scopeDecision struct {
	full     bool
	adapters []string
}

// adapterManifest and adapterFile split the two things a path under
// adapters/ can be: the manifest that states which versions are claimed,
// and everything else that adapter owns.
var (
	adapterManifest = regexp.MustCompile(`^adapters/[^/]+/adapter\.json$`)
	adapterFile     = regexp.MustCompile(`^adapters/([^/]+)/`)
)

// widensMatrix reports whether a changed path is one no run may narrow
// around: an adapter manifest is the claim itself, and the rest is the
// machinery that turns a claim into a job. A pull request touching any of
// them proves the whole matrix before it merges (docs/engine-versions.md
// §3).
func widensMatrix(path string) bool {
	return adapterManifest.MatchString(path) ||
		strings.HasPrefix(path, "internal/tools/versionmatrix/") ||
		path == "internal/capabilities/adapters.go" ||
		path == ".github/workflows/version-matrix.yml"
}

// bookkeeping reports the files an adapter change carries with it rather
// than chooses: the generated capability statement — rewritten because
// the adapter version had to move — and the changelog. Neither can alter
// what a restore job does, and neither may widen a run. Without this the
// narrowing would fire for test-only pull requests alone, since any
// change to an adapter's source must move its version and therefore
// regenerates docs/capabilities.json.
func bookkeeping(path string) bool {
	return path == "docs/capabilities.json" || path == "CHANGELOG.md"
}

// scopeFrom decides how wide a run has to be. Narrowing is deliberately
// the last resort: it needs every remaining path to sit under an adapter
// directory that carries a manifest, so a work-in-progress directory, a
// shared-code change, or a path nobody anticipated keeps the full
// baseline set rather than quietly shrinking the gate.
func scopeFrom(root string, changed []string) scopeDecision {
	var touched []string
	seen := map[string]bool{}
	local, any := true, false
	for _, path := range changed {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if widensMatrix(path) {
			return scopeDecision{full: true}
		}
		if bookkeeping(path) {
			continue
		}
		any = true
		match := adapterFile.FindStringSubmatch(path)
		if match == nil {
			local = false
			continue
		}
		if !seen[match[1]] {
			seen[match[1]] = true
			touched = append(touched, match[1])
		}
	}
	if !any || !local {
		return scopeDecision{}
	}
	for _, id := range touched {
		info, err := os.Stat(filepath.Join(root, "adapters", id, capabilities.ManifestFile))
		if err != nil || info.IsDir() {
			return scopeDecision{}
		}
	}
	sort.Strings(touched)
	return scopeDecision{adapters: touched}
}

// runScope writes the scope step's outputs in the key=value form
// $GITHUB_OUTPUT takes. An absent adapters key means every baseline: the
// workflow reads it that way, so a decision this tool cannot make never
// arrives as an empty filter the matrix would silently accept.
func runScope(root string, stdin io.Reader, stdout io.Writer) error {
	var changed []string
	scanner := bufio.NewScanner(stdin)
	for scanner.Scan() {
		changed = append(changed, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read changed paths: %w", err)
	}
	decision := scopeFrom(root, changed)
	if decision.full {
		fmt.Fprintln(stdout, "full=1")
		return nil
	}
	fmt.Fprintln(stdout, "full=0")
	if len(decision.adapters) > 0 {
		fmt.Fprintf(stdout, "adapters=%s\n", strings.Join(decision.adapters, ","))
	}
	return nil
}

// enumerate reads every adapter manifest and flattens it into one job per
// verified engine version, in adapter order and then manifest order.
func enumerate(root string) ([]target, error) {
	dirs, err := capabilities.AdapterDirs(root)
	if err != nil {
		return nil, err
	}
	var targets []target
	for _, dir := range dirs {
		m, err := capabilities.LoadAdapterManifest(dir)
		if err != nil {
			return nil, err
		}
		name := filepath.Base(dir)
		if len(m.Verified) == 0 {
			return nil, fmt.Errorf("adapter %s lists no verified engine version", name)
		}
		for _, v := range m.Verified {
			targets = append(targets, target{
				Adapter:       name,
				EngineVersion: v.EngineVersion,
				Image:         v.Image,
				Baseline:      v.Baseline,
			})
		}
	}
	return targets, nil
}
