package capabilities_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/probavi/probavi/internal/capabilities"
)

// build_test.go proves the generator refuses to publish a claim it cannot
// stand behind. Every case here is a way an adapter and its manifest can
// disagree; each must be a build failure rather than a plausible-looking
// entry on the website.

// fixtureAdapter is one synthetic adapter directory.
type fixtureAdapter struct {
	// manifest is written to adapter.json; nil writes no manifest.
	manifest map[string]any
	// probe is written to testdata/probe_response.golden; nil writes none.
	probe map[string]any
}

// sources returns the manifest's source-name table, so cases that add or
// drop a name stay one-liners.
func (a *fixtureAdapter) sources(t *testing.T) map[string]any {
	t.Helper()
	m, ok := a.manifest["sources"].(map[string]any)
	if !ok {
		t.Fatal("fixture manifest has no sources table")
	}
	return m
}

// probePayload returns the probe's payload object.
func (a *fixtureAdapter) probePayload(t *testing.T) map[string]any {
	t.Helper()
	p, ok := a.probe["payload"].(map[string]any)
	if !ok {
		t.Fatal("fixture probe has no payload")
	}
	return p
}

// validManifest is the shape every case starts from.
func validManifest() map[string]any {
	return map[string]any{
		"id":                   "demo",
		"name":                 "Demo",
		"status":               "experimental",
		"since":                "0.4.0",
		"engine_name":          "Demo Engine",
		"conformance_verified": true,
		"docs":                 "docs/capabilities.md",
		"versions_checked":     "2026-08-27",
		"verified": []any{
			map[string]any{"engine_version": "16", "image": "demo:16", "baseline": true},
		},
		"sources": map[string]any{"dump": "One demo dump file"},
	}
}

// validProbe mirrors the shape an adapter's probe golden really has.
func validProbe() map[string]any {
	return map[string]any{
		"ok":         true,
		"protocol":   "probavi-adapter/0",
		"request_id": "r-test",
		"payload": map[string]any{
			"name":              "demo",
			"adapter_version":   "1.0.0",
			"protocol_versions": []any{"probavi-adapter/0"},
			"engine":            map[string]any{"name": "demo"},
			"sources": []any{
				map[string]any{"kind": "dump", "capabilities": map[string]any{"pitr": false}},
			},
		},
	}
}

// fixtureRoot builds a repository root whose adapters/ is synthetic and
// whose documents are the real ones, so the cases exercise adapter
// validation without having to fake the whole repository.
func fixtureRoot(t *testing.T, adapters map[string]fixtureAdapter) string {
	t.Helper()
	real, err := filepath.Abs(repoRoot)
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	root := t.TempDir()
	for _, shared := range []string{"docs", "spec"} {
		if lerr := os.Symlink(filepath.Join(real, shared), filepath.Join(root, shared)); lerr != nil {
			t.Fatalf("link %s: %v", shared, lerr)
		}
	}
	if merr := os.MkdirAll(filepath.Join(root, "adapters"), 0o755); merr != nil {
		t.Fatalf("create adapters dir: %v", merr)
	}
	for name, a := range adapters {
		dir := filepath.Join(root, "adapters", name)
		if merr := os.MkdirAll(filepath.Join(dir, "testdata"), 0o755); merr != nil {
			t.Fatalf("create %s: %v", name, merr)
		}
		if a.manifest != nil {
			writeJSON(t, filepath.Join(dir, capabilities.ManifestFile), a.manifest)
		}
		if a.probe != nil {
			writeJSON(t, filepath.Join(dir, "testdata", "probe_response.golden"), a.probe)
		}
	}
	return root
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		t.Fatalf("write fixture %s: %v", path, err)
	}
}

func TestBuildRejectsInconsistentAdapters(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(t *testing.T, a *fixtureAdapter)
		wantErr string
	}{
		{
			name:    "source kind the manifest does not name",
			mutate:  func(t *testing.T, a *fixtureAdapter) { delete(a.sources(t), "dump") },
			wantErr: "unnamed",
		},
		{
			name: "manifest names a source the probe dropped",
			mutate: func(t *testing.T, a *fixtureAdapter) {
				a.sources(t)["removed_kind"] = "A kind that no longer exists"
			},
			wantErr: "does not declare",
		},
		{
			name:    "manifest missing entirely",
			mutate:  func(_ *testing.T, a *fixtureAdapter) { a.manifest = nil },
			wantErr: "read adapter manifest",
		},
		{
			name:    "manifest id disagrees with the directory",
			mutate:  func(_ *testing.T, a *fixtureAdapter) { a.manifest["id"] = "something-else" },
			wantErr: "does not match its directory",
		},
		{
			name: "probe name disagrees with the directory",
			mutate: func(t *testing.T, a *fixtureAdapter) {
				a.probePayload(t)["name"] = "other"
			},
			wantErr: "is not its directory",
		},
		{
			name:    "unknown maturity value",
			mutate:  func(_ *testing.T, a *fixtureAdapter) { a.manifest["status"] = "production-ready" },
			wantErr: "is not one of",
		},
		{
			// An omitted key decodes to the same nil an explicit null
			// does, and only one of the two is a statement.
			name:    "no since field at all",
			mutate:  func(_ *testing.T, a *fixtureAdapter) { delete(a.manifest, "since") },
			wantErr: "no since field",
		},
		{
			name:    "since that is not a release",
			mutate:  func(_ *testing.T, a *fixtureAdapter) { a.manifest["since"] = "0.4" },
			wantErr: "is not a release of this repository",
		},
		{
			name:    "no versions_checked",
			mutate:  func(_ *testing.T, a *fixtureAdapter) { delete(a.manifest, "versions_checked") },
			wantErr: "no versions_checked",
		},
		{
			name:    "versions_checked that is not a date",
			mutate:  func(_ *testing.T, a *fixtureAdapter) { a.manifest["versions_checked"] = "August 2026" },
			wantErr: "is not a YYYY-MM-DD date",
		},
		{
			// A vendor window nobody can parse is a window nothing can
			// act on when it closes.
			name: "supported_until that is not a date",
			mutate: func(_ *testing.T, a *fixtureAdapter) {
				a.manifest["verified"] = []any{
					map[string]any{"engine_version": "16", "image": "demo:16", "baseline": true,
						"supported_until": "next November"},
				}
			},
			wantErr: "not a YYYY-MM-DD date",
		},
		{
			name:    "no display name",
			mutate:  func(_ *testing.T, a *fixtureAdapter) { a.manifest["name"] = "" },
			wantErr: "declares no name",
		},
		{
			name:    "no engine display name",
			mutate:  func(_ *testing.T, a *fixtureAdapter) { a.manifest["engine_name"] = "" },
			wantErr: "declares no engine_name",
		},
		{
			name:    "no verified engine version",
			mutate:  func(_ *testing.T, a *fixtureAdapter) { a.manifest["verified"] = []any{} },
			wantErr: "no verified engine version",
		},
		{
			name: "verified entry missing its image",
			mutate: func(_ *testing.T, a *fixtureAdapter) {
				a.manifest["verified"] = []any{map[string]any{"engine_version": "16", "image": ""}}
			},
			wantErr: "needs both engine_version and image",
		},
		{
			name: "engine version absent from the image CI pulls",
			mutate: func(_ *testing.T, a *fixtureAdapter) {
				a.manifest["verified"] = []any{map[string]any{"engine_version": "17", "image": "demo:16"}}
			},
			wantErr: "does not appear in image tag",
		},
		{
			// An engine that ships as a library states its version in the
			// artifact instead, because no image tag can carry it
			// (docs/engine-versions.md §1).
			name: "engine version absent from the artifact CI fetches",
			mutate: func(_ *testing.T, a *fixtureAdapter) {
				a.manifest["verified"] = []any{map[string]any{
					"engine_version": "2.4.240", "image": "jre:21",
					"engine_artifact": "https://example.invalid/h2-2.3.232.jar", "baseline": true}}
			},
			wantErr: "does not appear in engine_artifact",
		},
		{
			// Uniqueness follows the engine, not the base: two artifacts
			// on one image are the ordinary shape here, two entries of one
			// artifact are the same job claimed twice.
			name: "the same artifact listed twice",
			mutate: func(_ *testing.T, a *fixtureAdapter) {
				a.manifest["verified"] = []any{
					map[string]any{"engine_version": "2.4.240", "image": "jre:21",
						"engine_artifact": "https://example.invalid/h2-2.4.240.jar", "baseline": true},
					map[string]any{"engine_version": "2.4.240", "image": "jre:21",
						"engine_artifact": "https://example.invalid/h2-2.4.240.jar"},
				}
			},
			wantErr: "is listed twice",
		},
		{
			// Without a baseline the everyday integration job has no image
			// to restore from, and the matrix would rest on whichever
			// entry happened to be written first.
			name: "no entry marked baseline",
			mutate: func(_ *testing.T, a *fixtureAdapter) {
				a.manifest["verified"] = []any{map[string]any{"engine_version": "16", "image": "demo:16"}}
			},
			wantErr: "marked baseline, want exactly 1",
		},
		{
			name: "several entries marked baseline",
			mutate: func(_ *testing.T, a *fixtureAdapter) {
				a.manifest["verified"] = []any{
					map[string]any{"engine_version": "16", "image": "demo:16", "baseline": true},
					map[string]any{"engine_version": "17", "image": "demo:17", "baseline": true},
				}
			},
			wantErr: "marked baseline, want exactly 1",
		},
		{
			name: "the same engine version listed twice",
			mutate: func(_ *testing.T, a *fixtureAdapter) {
				a.manifest["verified"] = []any{
					map[string]any{"engine_version": "16", "image": "demo:16", "baseline": true},
					map[string]any{"engine_version": "16", "image": "other/demo:16"},
				}
			},
			wantErr: "is listed twice",
		},
		{
			name: "the same image listed twice",
			mutate: func(_ *testing.T, a *fixtureAdapter) {
				a.manifest["verified"] = []any{
					map[string]any{"engine_version": "16", "image": "demo:16.1", "baseline": true},
					map[string]any{"engine_version": "16.1", "image": "demo:16.1"},
				}
			},
			wantErr: "is listed twice",
		},
		{
			name:    "docs path that does not exist",
			mutate:  func(_ *testing.T, a *fixtureAdapter) { a.manifest["docs"] = "docs/no-such-file.md" },
			wantErr: "is not in the repository",
		},
		{
			name: "manifest that is not JSON",
			mutate: func(_ *testing.T, a *fixtureAdapter) {
				a.manifest = map[string]any{"id": "demo", "verified": "not-a-list"}
			},
			wantErr: "parse",
		},
		{
			name:    "probe golden missing",
			mutate:  func(_ *testing.T, a *fixtureAdapter) { a.probe = nil },
			wantErr: "but no testdata/probe_response.golden",
		},
		{
			name: "probe golden reports a failed probe",
			mutate: func(_ *testing.T, a *fixtureAdapter) {
				a.probe["ok"] = false
			},
			wantErr: "not a successful probe response",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := fixtureAdapter{manifest: validManifest(), probe: validProbe()}
			tc.mutate(t, &a)
			root := fixtureRoot(t, map[string]fixtureAdapter{"demo": a})
			_, err := capabilities.Build(root)
			if err == nil {
				t.Fatal("Build accepted an adapter it must reject")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestBuildAcceptsAConsistentAdapter is the control: without a mutation
// the same fixture builds, so the cases above fail for the reason they
// claim and not because the fixture is broken.
func TestBuildAcceptsAConsistentAdapter(t *testing.T) {
	root := fixtureRoot(t, map[string]fixtureAdapter{
		"demo": {manifest: validManifest(), probe: validProbe()},
	})
	doc, err := capabilities.Build(root)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(doc.Adapters) != 1 || doc.Adapters[0].ID != "demo" {
		t.Fatalf("document adapters = %+v", doc.Adapters)
	}
	a := doc.Adapters[0]
	if a.AdapterVersion != "1.0.0" || a.Engine.ID != "demo" || a.Engine.Name != "Demo Engine" {
		t.Errorf("adapter entry did not join probe and manifest: %+v", a)
	}
	if len(a.Sources) != 1 || a.Sources[0].Name != "One demo dump file" || a.PITR {
		t.Errorf("sources did not join probe and manifest: %+v", a.Sources)
	}
	if a.Since == nil || *a.Since != "0.4.0" {
		t.Errorf("since = %v, want the release the manifest states", a.Since)
	}
}

// TestBuildPublishesAnUnreleasedAdapterAsNull is the other half of the
// since field: an adapter that is in the tree but not in any release says
// so, and says it as a value rather than by leaving the key out
// (docs/capabilities.md §1.5).
func TestBuildPublishesAnUnreleasedAdapterAsNull(t *testing.T) {
	m := validManifest()
	m["since"] = nil
	root := fixtureRoot(t, map[string]fixtureAdapter{
		"demo": {manifest: m, probe: validProbe()},
	})
	doc, err := capabilities.Build(root)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if doc.Adapters[0].Since != nil {
		t.Errorf("since = %v, want null", *doc.Adapters[0].Since)
	}
	rendered, err := capabilities.Render(doc)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(string(rendered), `"since": null`) {
		t.Error("rendered document omits the since key instead of writing null")
	}
}

// TestBuildRequiresAnAdapter guards the empty case: a repository with no
// adapters must fail rather than publish a manifest claiming none exist.
func TestBuildRequiresAnAdapter(t *testing.T) {
	root := fixtureRoot(t, nil)
	if _, err := capabilities.Build(root); err == nil {
		t.Fatal("Build accepted a repository with no adapters")
	}
}

// TestBuildRejectsAMissingAdaptersDirectory covers the unreadable root.
func TestBuildRejectsAMissingAdaptersDirectory(t *testing.T) {
	if _, err := capabilities.Build(t.TempDir()); err == nil {
		t.Fatal("Build accepted a root with no adapters directory")
	}
}

// TestBuildRejectsAMissingDocument proves the document-path check: the
// manifest must never publish a link the repository cannot honor.
func TestBuildRejectsAMissingDocument(t *testing.T) {
	root := fixtureRoot(t, map[string]fixtureAdapter{
		"demo": {manifest: validManifest(), probe: validProbe()},
	})
	// docs/ is a symlink to the real tree; replace it with one that is
	// missing the capabilities contract.
	if err := os.Remove(filepath.Join(root, "docs")); err != nil {
		t.Fatalf("unlink docs: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatalf("create empty docs: %v", err)
	}
	_, err := capabilities.Build(root)
	if err == nil {
		t.Fatal("Build accepted a repository with no normative documents")
	}
	if !strings.Contains(err.Error(), "is not in the repository") {
		t.Errorf("error %q does not name the missing document", err)
	}
}

// TestSandboxImageIsTheOneTestsUse pins the accessor the adapter
// integration suites call, which is what stops a verified version from
// being one CI never exercises.
func TestSandboxImageIsTheOneTestsUse(t *testing.T) {
	for _, dir := range adapterDirs(t) {
		m, err := capabilities.LoadAdapterManifest(dir)
		if err != nil {
			t.Fatalf("load %s: %v", dir, err)
		}
		image, err := m.SandboxImage("")
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		var baseline string
		for _, v := range m.Verified {
			if v.Baseline {
				baseline = v.Image
			}
		}
		if image != baseline {
			t.Errorf("%s: SandboxImage(\"\") returned %q, want the baseline %q", dir, image, baseline)
		}
		// The matrix names an image the manifest already lists...
		last := m.Verified[len(m.Verified)-1].Image
		if got, err := m.SandboxImage(last); err != nil || got != last {
			t.Errorf("%s: SandboxImage(%q) = %q, %v", dir, last, got, err)
		}
		// ...and anything else is refused, so a typo in a workflow cannot
		// publish a green run for a version this repository never claimed.
		if _, err := m.SandboxImage(last + "-typo"); err == nil {
			t.Errorf("%s: SandboxImage accepted an image the manifest does not list", dir)
		}
	}
	empty := &capabilities.AdapterManifest{ID: "demo"}
	if _, err := empty.SandboxImage(""); err == nil {
		t.Error("SandboxImage accepted a manifest with no baseline entry")
	}
}

func adapterDirs(t *testing.T) []string {
	t.Helper()
	dirs, err := capabilities.AdapterDirs(repoRoot)
	if err != nil {
		t.Fatalf("AdapterDirs: %v", err)
	}
	return dirs
}

// TestBuildRejectsAContractPathOfTheWrongKind covers the other half of the
// path check: a declared directory that is actually a file. The manifest
// tells consumers where the normative documents are, so a path that
// resolves to the wrong kind of thing is as broken as one that is missing.
func TestBuildRejectsAContractPathOfTheWrongKind(t *testing.T) {
	root := fixtureRoot(t, map[string]fixtureAdapter{
		"demo": {manifest: validManifest(), probe: validProbe()},
	})
	if err := os.Remove(filepath.Join(root, "spec")); err != nil {
		t.Fatalf("unlink spec: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "spec"), 0o755); err != nil {
		t.Fatalf("create spec: %v", err)
	}
	// The manifest declares spec/evidence as the independent verifier's
	// directory; make it a regular file instead.
	if err := os.WriteFile(filepath.Join(root, "spec", "evidence"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write spec/evidence: %v", err)
	}
	_, err := capabilities.Build(root)
	if err == nil {
		t.Fatal("Build accepted a contract path of the wrong kind")
	}
	if !strings.Contains(err.Error(), "is not a directory") {
		t.Errorf("error %q does not explain the mismatch", err)
	}
}

// TestBuildRejectsAdapterDocsPointingAtADirectory is the same check on the
// per-adapter side, where a manifest author is most likely to slip.
func TestBuildRejectsAdapterDocsPointingAtADirectory(t *testing.T) {
	m := validManifest()
	m["docs"] = "docs"
	root := fixtureRoot(t, map[string]fixtureAdapter{
		"demo": {manifest: m, probe: validProbe()},
	})
	_, err := capabilities.Build(root)
	if err == nil {
		t.Fatal("Build accepted adapter docs pointing at a directory")
	}
	if !strings.Contains(err.Error(), "is not a file") {
		t.Errorf("error %q does not explain the mismatch", err)
	}
}
