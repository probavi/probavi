//go:build integration

package conformance

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/probavi/probavi/internal/capabilities"
)

// TestInRepoAdaptersAreConformant is the Phase 2 exit criterion's other
// half: the shipped adapters pass the frozen §10 list. No container
// runtime is involved — conformance runs against the simulated sandbox.
//
// The list of adapters comes from the manifests that also feed
// docs/capabilities.json: every adapter whose adapter.json declares
// conformance_verified is driven here, so that published claim is one CI
// enforces rather than one a maintainer remembered to keep true.
func TestInRepoAdaptersAreConformant(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dirs, err := capabilities.AdapterDirs("../..")
	if err != nil {
		t.Fatalf("list adapters: %v", err)
	}
	tested := 0
	for _, dir := range dirs {
		m, merr := capabilities.LoadAdapterManifest(dir)
		if merr != nil {
			t.Fatalf("load manifest %s: %v", dir, merr)
		}
		if !m.ConformanceVerified {
			t.Logf("%s does not declare conformance_verified — skipped", m.ID)
			continue
		}
		tested++
		t.Run(m.ID, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), "probavi-adapter-"+m.ID)
			if out, berr := exec.CommandContext(ctx, "go", "build", "-o", bin,
				dir).CombinedOutput(); berr != nil {
				t.Fatalf("build adapter: %v: %s", berr, out)
			}
			// An adapter whose artifact is a directory cannot be driven
			// with the temporary file the suite generates, so it commits
			// the smallest artifact its host-side pass accepts and the
			// suite provisions from that.
			opts := Options{}
			fixture := filepath.Join(dir, "testdata", "conformance-source")
			if _, ferr := os.Stat(fixture); ferr == nil {
				opts.SourcePath = fixture
			}
			report, rerr := Run(ctx, bin, opts)
			if rerr != nil {
				t.Fatalf("Run: %v", rerr)
			}
			for _, c := range report.Checks {
				if !c.OK {
					t.Errorf("FAIL %s: %s", c.Name, c.Detail)
				}
			}
			if report.Failed != 0 || report.Passed != 15 {
				t.Fatalf("report: %d passed / %d failed", report.Passed, report.Failed)
			}
		})
	}
	if tested == 0 {
		t.Fatal("no adapter declares conformance_verified — the suite would pass vacuously")
	}
}
