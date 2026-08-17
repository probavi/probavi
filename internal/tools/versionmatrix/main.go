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
// It is a repository tool, not a shipped binary.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "versionmatrix: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("versionmatrix", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", ".", "repository root")
	baselinesOnly := fs.Bool("baselines-only", false,
		"emit only each adapter's baseline version — the everyday gate")
	adapters := fs.String("adapters", "",
		"comma-separated adapter ids to keep — the scope of an adapter-local pull request; "+
			"empty keeps every adapter, and an id no manifest declares is an error")
	if err := fs.Parse(args); err != nil {
		return err
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
