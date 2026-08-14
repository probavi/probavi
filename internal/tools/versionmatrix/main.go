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
	"os"
	"path/filepath"

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
	root := flag.String("root", ".", "repository root")
	baselinesOnly := flag.Bool("baselines-only", false,
		"emit only each adapter's baseline version — the everyday gate")
	flag.Parse()

	targets, err := enumerate(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "versionmatrix: %v\n", err)
		os.Exit(1)
	}
	if *baselinesOnly {
		targets = baselines(targets)
	}
	out, err := json.Marshal(targets)
	if err != nil {
		fmt.Fprintf(os.Stderr, "versionmatrix: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(out))
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
