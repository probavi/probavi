package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/probavi/probavi/internal/capabilities"
)

// writeAdapter lays out the two files AdapterDirs looks for. The golden
// may be empty: this tool never reads it, and requiring a valid one would
// couple the test to the protocol's response schema for no gain.
func writeAdapter(t *testing.T, root, name, manifest string) {
	t.Helper()
	dir := filepath.Join(root, "adapters", name)
	if err := os.MkdirAll(filepath.Join(dir, "testdata"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "testdata", "probe_response.golden"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, capabilities.ManifestFile), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestEnumerate(t *testing.T) {
	tests := []struct {
		name      string
		manifests map[string]string
		want      []target
		wantErr   string
	}{
		{
			name: "one job per verified entry, adapter order then manifest order",
			manifests: map[string]string{
				"zebra": `{"id":"zebra","verified":[{"engine_version":"3","image":"z:3","baseline":true}]}`,
				"alpha": `{"id":"alpha","verified":[
					{"engine_version":"1","image":"a:1"},
					{"engine_version":"2","image":"a:2","baseline":true}]}`,
			},
			want: []target{
				{Adapter: "alpha", EngineVersion: "1", Image: "a:1"},
				{Adapter: "alpha", EngineVersion: "2", Image: "a:2", Baseline: true},
				{Adapter: "zebra", EngineVersion: "3", Image: "z:3", Baseline: true},
			},
		},
		{
			// A manifest with an empty list would silently shrink the
			// matrix, which is the one failure mode this tool exists to
			// make impossible.
			name:      "an adapter with no verified entry is an error",
			manifests: map[string]string{"alpha": `{"id":"alpha","verified":[]}`},
			wantErr:   "lists no verified engine version",
		},
		{
			name:      "an unparseable manifest is an error",
			manifests: map[string]string{"alpha": `{`},
			wantErr:   "parse",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for name, manifest := range tc.manifests {
				writeAdapter(t, root, name, manifest)
			}
			got, err := enumerate(root)
			if tc.wantErr != "" {
				assertErrorContains(t, err, tc.wantErr)
				return
			}
			if err != nil {
				t.Fatalf("enumerate: %v", err)
			}
			assertTargets(t, got, tc.want)
		})
	}
}

func assertErrorContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("enumerate error = %v, want one containing %q", err, want)
	}
}

func assertTargets(t *testing.T, got, want []target) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d targets, want %d: %+v", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("target %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestRunPrintsTheMatrix covers the exact invocations the workflow uses:
// the full matrix and the -baselines-only everyday gate, both as one JSON
// array on stdout.
func TestRunPrintsTheMatrix(t *testing.T) {
	root := filepath.Join("..", "..", "..")

	var full bytes.Buffer
	if err := run([]string{"-root", root}, &full, io.Discard); err != nil {
		t.Fatalf("run: %v", err)
	}
	var all []target
	if err := json.Unmarshal(full.Bytes(), &all); err != nil {
		t.Fatalf("output is not a JSON matrix: %v (%q)", err, full.String())
	}
	if len(all) == 0 {
		t.Fatal("empty matrix — the repository has adapters, so this cannot be right")
	}

	var everyday bytes.Buffer
	if err := run([]string{"-root", root, "-baselines-only"}, &everyday, io.Discard); err != nil {
		t.Fatalf("run -baselines-only: %v", err)
	}
	var base []target
	if err := json.Unmarshal(everyday.Bytes(), &base); err != nil {
		t.Fatalf("baselines output is not a JSON matrix: %v", err)
	}
	if len(base) == 0 || len(base) >= len(all) {
		t.Errorf("baselines emitted %d of %d targets, want a proper non-empty subset", len(base), len(all))
	}

	if err := run([]string{"-root", filepath.Join(t.TempDir(), "absent")}, io.Discard, io.Discard); err == nil {
		t.Error("run with a missing root must fail")
	}
}

// TestRunScopesToNamedAdapters pins the -adapters filter an adapter-local
// pull request's workflow run passes: only the named adapters' targets
// survive, enumeration order is preserved regardless of the order the
// caller named them, and a name no manifest declares — the typo that
// would silently shrink the matrix — is an error rather than a smaller
// green run.
func TestRunScopesToNamedAdapters(t *testing.T) {
	root := t.TempDir()
	writeAdapter(t, root, "alpha", `{"id":"alpha","verified":[
		{"engine_version":"1","image":"a:1"},
		{"engine_version":"2","image":"a:2","baseline":true}]}`)
	writeAdapter(t, root, "mid", `{"id":"mid","verified":[{"engine_version":"5","image":"m:5","baseline":true}]}`)
	writeAdapter(t, root, "zebra", `{"id":"zebra","verified":[{"engine_version":"3","image":"z:3","baseline":true}]}`)

	tests := []struct {
		name    string
		args    []string
		want    []target
		wantErr string
	}{
		{
			name: "one adapter's baseline — the adapter-local everyday gate",
			args: []string{"-root", root, "-baselines-only", "-adapters", "alpha"},
			want: []target{{Adapter: "alpha", EngineVersion: "2", Image: "a:2", Baseline: true}},
		},
		{
			name: "two adapters keep enumeration order, not naming order",
			args: []string{"-root", root, "-adapters", "zebra,alpha"},
			want: []target{
				{Adapter: "alpha", EngineVersion: "1", Image: "a:1"},
				{Adapter: "alpha", EngineVersion: "2", Image: "a:2", Baseline: true},
				{Adapter: "zebra", EngineVersion: "3", Image: "z:3", Baseline: true},
			},
		},
		{
			name:    "an unknown adapter is an error, not a smaller matrix",
			args:    []string{"-root", root, "-adapters", "alpha,ghost"},
			wantErr: `"ghost"`,
		},
		{
			name:    "an empty id is an error",
			args:    []string{"-root", root, "-adapters", "alpha,"},
			wantErr: "empty adapter id",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := run(tc.args, &out, io.Discard)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("run error = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			var got []target
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatalf("output is not a JSON matrix: %v (%q)", err, out.String())
			}
			assertTargets(t, got, tc.want)
		})
	}
}

// TestMatrixCoversEveryClaimedVersion is the gate that matters: the jobs
// this tool prints for the real repository are exactly the versions
// docs/capabilities.json claims CI restores from. If they ever diverge,
// one of the two is lying.
func TestMatrixCoversEveryClaimedVersion(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	targets, err := enumerate(root)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	if len(targets) == 0 {
		t.Fatal("no matrix targets — the repository has adapters, so this cannot be right")
	}

	raw, err := os.ReadFile(filepath.Join(root, "docs", "capabilities.json"))
	if err != nil {
		t.Fatalf("read capabilities.json: %v", err)
	}
	var doc struct {
		Adapters []struct {
			ID       string `json:"id"`
			Verified []struct {
				EngineVersion string `json:"engine_version"`
				Image         string `json:"image"`
			} `json:"verified"`
		} `json:"adapters"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse capabilities.json: %v", err)
	}

	claimed := map[string]bool{}
	for _, a := range doc.Adapters {
		for _, v := range a.Verified {
			claimed[a.ID+" "+v.EngineVersion+" "+v.Image] = true
		}
	}
	scheduled := map[string]bool{}
	for _, tgt := range targets {
		scheduled[tgt.Adapter+" "+tgt.EngineVersion+" "+tgt.Image] = true
	}
	for key := range claimed {
		if !scheduled[key] {
			t.Errorf("capabilities.json claims %q but no matrix job restores from it", key)
		}
	}
	for key := range scheduled {
		if !claimed[key] {
			t.Errorf("matrix job %q restores from a version capabilities.json does not claim", key)
		}
	}
}

// TestBaselinesCoverEveryAdapterExactlyOnce pins what -baselines-only
// emits: the everyday gate must lose versions, never adapters. A filter
// that quietly dropped an adapter would leave an engine untested on every
// push while the run still went green.
func TestBaselinesCoverEveryAdapterExactlyOnce(t *testing.T) {
	all, err := enumerate(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	adapters := map[string]bool{}
	for _, t := range all {
		adapters[t.Adapter] = true
	}

	got := baselines(all)
	if len(got) != len(adapters) {
		t.Fatalf("got %d baselines for %d adapters", len(got), len(adapters))
	}
	seen := map[string]int{}
	for _, tgt := range got {
		if !tgt.Baseline {
			t.Errorf("%s %s is not a baseline", tgt.Adapter, tgt.EngineVersion)
		}
		seen[tgt.Adapter]++
	}
	for adapter := range adapters {
		if seen[adapter] != 1 {
			t.Errorf("adapter %s appears %d times in the baseline set, want 1", adapter, seen[adapter])
		}
	}
}

// TestExactlyOneBaselinePerAdapter pins what the everyday integration job
// runs: one version per adapter, chosen in the manifest rather than by
// list order.
func TestExactlyOneBaselinePerAdapter(t *testing.T) {
	targets, err := enumerate(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	baselines := map[string]int{}
	for _, tgt := range targets {
		if tgt.Baseline {
			baselines[tgt.Adapter]++
		}
	}
	seen := map[string]bool{}
	for _, tgt := range targets {
		seen[tgt.Adapter] = true
	}
	for adapter := range seen {
		if baselines[adapter] != 1 {
			t.Errorf("adapter %s has %d baseline versions, want exactly 1", adapter, baselines[adapter])
		}
	}
}
