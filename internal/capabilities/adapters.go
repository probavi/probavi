package capabilities

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// adapters.go reads the two per-adapter sources of truth and refuses to
// let them disagree.
//
// The probe golden is the implementation: TestProbeGolden in each adapter
// writes it from the live probe payload through the real response encoder,
// so the adapter version, engine id, source kinds, and pitr capability
// here are the adapter's own answers, gated by CI. The manifest carries
// only what the probe physically cannot — the probe payload schema is
// closed (docs/schemas/adapter/probe-response.json), and widening it would
// be a protocol change.

const (
	// ManifestFile is the per-adapter manifest, next to the adapter it
	// describes.
	ManifestFile = "adapter.json"
	// probeGoldenFile is the adapter's committed probe response.
	probeGoldenFile = "testdata/probe_response.golden"
)

// AdapterManifest is adapters/<id>/adapter.json: the facts about an
// adapter that its probe response cannot carry.
type AdapterManifest struct {
	// ID must equal the adapter's directory name and the name its probe
	// declares.
	ID string `json:"id"`
	// Name is the adapter's display name.
	Name string `json:"name"`
	// Status is one of the capabilities maturity values.
	Status string `json:"status"`
	// Since is the release this repository first shipped the adapter in,
	// and null while it has not been released yet. It dates the adapter
	// rather than the engine: an engine already restorable through a
	// different adapter keeps the later release here, and the adapter's
	// README records the earlier coverage. internal/docs holds the value
	// to CHANGELOG.md.
	Since Release `json:"since"`
	// EngineName is the engine's display name; its id comes from the probe.
	EngineName string `json:"engine_name"`
	// ConformanceVerified records that CI drives this adapter through the
	// frozen protocol conformance suite. The conformance integration test
	// iterates the adapters that declare it, so it cannot be aspirational.
	ConformanceVerified bool `json:"conformance_verified"`
	// Docs is the repository-relative path to the adapter's README.
	Docs string `json:"docs"`
	// Verified lists the engine versions the integration suite restores
	// from. The suite reads Image from here, so a version listed but never
	// exercised is not possible.
	Verified []ManifestTarget `json:"verified"`
	// Sources maps every source kind the probe declares to its display
	// name. Build fails on any mismatch in either direction.
	Sources map[string]string `json:"sources"`
}

// LoadAdapterManifest reads the manifest in an adapter's directory.
func LoadAdapterManifest(dir string) (*AdapterManifest, error) {
	path := filepath.Join(dir, ManifestFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read adapter manifest: %w", err)
	}
	m := &AdapterManifest{}
	if err := json.Unmarshal(raw, m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return m, nil
}

// Release is the value of a manifest's since field. An absent key and an
// explicit null both leave no version behind, and they do not mean the
// same thing: one says the adapter has not been released, the other says
// nobody wrote it down. Only the first is a statement, so the key's
// presence is recorded rather than inferred (docs/capabilities.md §1.5).
type Release struct {
	// Stated reports that the manifest carried the key at all.
	Stated bool
	// Value is the release, or empty for an explicit null.
	Value string
}

// UnmarshalJSON records that the key was present, whatever it held.
func (r *Release) UnmarshalJSON(raw []byte) error {
	r.Stated = true
	if string(raw) == "null" {
		return nil
	}
	return json.Unmarshal(raw, &r.Value)
}

// Published renders the field for docs/capabilities.json, where an absent
// value is null and never an omitted key.
func (r Release) Published() *string {
	if r.Value == "" {
		return nil
	}
	v := r.Value
	return &v
}

// ManifestTarget is one engine version in an adapter's manifest. It is the
// published VerifiedTarget plus the scheduling flag that decides when CI
// exercises it, which is a fact about this repository's pipeline rather
// than a capability — so it stays out of docs/capabilities.json.
type ManifestTarget struct {
	EngineVersion string `json:"engine_version"`
	Image         string `json:"image"`
	// Baseline marks the one version every run restores from. The rest are
	// exercised by the scheduled version matrix and before a release tag
	// (docs/engine-versions.md §3).
	Baseline bool `json:"baseline,omitempty"`
}

// BaselineImage returns the image every integration run restores from.
func (m *AdapterManifest) BaselineImage() (string, error) {
	for _, v := range m.Verified {
		if v.Baseline && v.Image != "" {
			return v.Image, nil
		}
	}
	return "", fmt.Errorf("adapter %s: manifest marks no verified entry as the baseline", m.ID)
}

// SandboxImage resolves the engine image an integration run must use.
//
// An empty request selects the baseline — the everyday case. A non-empty
// one comes from PROBAVI_IT_IMAGE, which the version-matrix workflow sets
// per job, and it is accepted only if the manifest lists it: a matrix that
// could name any image would let CI report a green run for a version this
// repository never claimed, which is the inverse of what the manifest is
// for (docs/engine-versions.md §2).
func (m *AdapterManifest) SandboxImage(requested string) (string, error) {
	if requested == "" {
		return m.BaselineImage()
	}
	for _, v := range m.Verified {
		if v.Image == requested {
			return v.Image, nil
		}
	}
	return "", fmt.Errorf("adapter %s: image %q is not one of its verified entries — "+
		"add it to %s before testing against it", m.ID, requested, ManifestFile)
}

// probeGolden is the committed probe response of an adapter, in the shape
// the adapter protocol's response and probe payload schemas define.
type probeGolden struct {
	OK      bool `json:"ok"`
	Payload struct {
		Name             string   `json:"name"`
		AdapterVersion   string   `json:"adapter_version"`
		ProtocolVersions []string `json:"protocol_versions"`
		Engine           struct {
			Name string `json:"name"`
		} `json:"engine"`
		Sources []struct {
			Kind         string `json:"kind"`
			Capabilities struct {
				PITR bool `json:"pitr"`
			} `json:"capabilities"`
		} `json:"sources"`
	} `json:"payload"`
}

func loadProbeGolden(dir string) (*probeGolden, error) {
	path := filepath.Join(dir, filepath.FromSlash(probeGoldenFile))
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read probe golden: %w", err)
	}
	g := &probeGolden{}
	if err := json.Unmarshal(raw, g); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if !g.OK || g.Payload.Name == "" {
		return nil, fmt.Errorf("%s: not a successful probe response", path)
	}
	return g, nil
}

// AdapterDirs lists the adapter directories of a repository, sorted. An
// adapter is a directory under adapters/ that carries a probe golden; a
// directory with a manifest but no golden is an error, not a silent skip.
func AdapterDirs(root string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(root, "adapters"))
	if err != nil {
		return nil, fmt.Errorf("list adapters: %w", err)
	}
	var dirs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, "adapters", e.Name())
		hasGolden := exists(filepath.Join(dir, filepath.FromSlash(probeGoldenFile)))
		hasManifest := exists(filepath.Join(dir, ManifestFile))
		switch {
		case hasGolden:
			dirs = append(dirs, dir)
		case hasManifest:
			return nil, fmt.Errorf("adapter %s has %s but no %s: run its probe golden test",
				e.Name(), ManifestFile, probeGoldenFile)
		}
	}
	sort.Strings(dirs)
	return dirs, nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// buildAdapter turns one adapter directory into a document entry, failing
// on any disagreement between the manifest and the adapter's own probe.
func buildAdapter(root, dir string) (Adapter, error) {
	name := filepath.Base(dir)
	golden, err := loadProbeGolden(dir)
	if err != nil {
		return Adapter{}, fmt.Errorf("adapter %s: %w", name, err)
	}
	m, err := LoadAdapterManifest(dir)
	if err != nil {
		return Adapter{}, fmt.Errorf("adapter %s: %w", name, err)
	}
	if err := checkAdapterIdentity(name, m, golden); err != nil {
		return Adapter{}, err
	}
	sources, pitr, err := buildSources(name, m, golden)
	if err != nil {
		return Adapter{}, err
	}
	if err := checkVerified(name, m); err != nil {
		return Adapter{}, err
	}
	if err := requireFile(root, m.Docs, fmt.Sprintf("adapter %s docs", name)); err != nil {
		return Adapter{}, err
	}
	return Adapter{
		ID:                  m.ID,
		Name:                m.Name,
		Status:              m.Status,
		Since:               m.Since.Published(),
		AdapterVersion:      golden.Payload.AdapterVersion,
		Engine:              Engine{ID: golden.Payload.Engine.Name, Name: m.EngineName},
		ProtocolVersions:    golden.Payload.ProtocolVersions,
		Verified:            publishedTargets(m.Verified),
		Sources:             sources,
		PITR:                pitr,
		ConformanceVerified: m.ConformanceVerified,
		Docs:                nullable(m.Docs),
	}, nil
}

// publishedTargets drops the scheduling flag: a consumer is told which
// engine versions CI restores from, not which of them ran on the last
// push. Order follows the manifest so the generated bytes stay stable.
func publishedTargets(targets []ManifestTarget) []VerifiedTarget {
	out := make([]VerifiedTarget, 0, len(targets))
	for _, t := range targets {
		out = append(out, VerifiedTarget{EngineVersion: t.EngineVersion, Image: t.Image})
	}
	return out
}

func checkAdapterIdentity(dirName string, m *AdapterManifest, golden *probeGolden) error {
	switch {
	case m.ID != dirName:
		return fmt.Errorf("adapter %s: manifest id %q does not match its directory", dirName, m.ID)
	case golden.Payload.Name != dirName:
		return fmt.Errorf("adapter %s: probe declares name %q, which is not its directory", dirName, golden.Payload.Name)
	case m.Name == "":
		return fmt.Errorf("adapter %s: manifest declares no name", dirName)
	case m.EngineName == "":
		return fmt.Errorf("adapter %s: manifest declares no engine_name", dirName)
	case !validStatus(m.Status):
		return fmt.Errorf("adapter %s: status %q is not one of %s", dirName, m.Status, strings.Join(Statuses(), ", "))
	case !m.Since.Stated:
		return fmt.Errorf("adapter %s: no since field — state the release this adapter first shipped in, "+
			"or null while it has not been released", dirName)
	case m.Since.Value != "" && !releasePattern.MatchString(m.Since.Value):
		return fmt.Errorf("adapter %s: since %q is not a release of this repository (x.y.z); "+
			"an adapter that has not been released yet states null", dirName, m.Since.Value)
	}
	return nil
}

// releasePattern matches a release of this repository. The value is only
// checked for shape here — whether it is the *right* release is a question
// about CHANGELOG.md, which internal/docs answers.
var releasePattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// buildSources joins the probe's source kinds to the manifest's display
// names. A kind the adapter declares but the manifest does not name — and
// a name for a kind the adapter no longer declares — both fail: that is
// what makes adding a source kind a change the generator notices.
func buildSources(dirName string, m *AdapterManifest, golden *probeGolden) ([]Source, bool, error) {
	sources := make([]Source, 0, len(golden.Payload.Sources))
	anyPITR := false
	declared := map[string]bool{}
	for _, s := range golden.Payload.Sources {
		declared[s.Kind] = true
		display, ok := m.Sources[s.Kind]
		if !ok || display == "" {
			return nil, false, fmt.Errorf("adapter %s: source kind %q is declared by the probe but unnamed in %s",
				dirName, s.Kind, ManifestFile)
		}
		sources = append(sources, Source{ID: s.Kind, Name: display, PITR: s.Capabilities.PITR})
		anyPITR = anyPITR || s.Capabilities.PITR
	}
	for kind := range m.Sources {
		if !declared[kind] {
			return nil, false, fmt.Errorf("adapter %s: %s names source kind %q, which the probe does not declare",
				dirName, ManifestFile, kind)
		}
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].ID < sources[j].ID })
	return sources, anyPITR, nil
}

// checkVerified pins the declared engine version to the image the
// integration suite actually pulls: the version must appear in the image
// reference, so a mislabelled version cannot reach the manifest.
func checkVerified(dirName string, m *AdapterManifest) error {
	if len(m.Verified) == 0 {
		return fmt.Errorf("adapter %s: manifest lists no verified engine version", dirName)
	}
	baselines := 0
	seenVersion := make(map[string]bool, len(m.Verified))
	seenImage := make(map[string]bool, len(m.Verified))
	for _, v := range m.Verified {
		if v.EngineVersion == "" || v.Image == "" {
			return fmt.Errorf("adapter %s: verified entry needs both engine_version and image", dirName)
		}
		tag := v.Image
		if i := strings.LastIndex(v.Image, ":"); i >= 0 {
			tag = v.Image[i+1:]
		}
		if !strings.Contains(tag, v.EngineVersion) {
			return fmt.Errorf("adapter %s: verified engine_version %q does not appear in image tag %q",
				dirName, v.EngineVersion, v.Image)
		}
		// Two entries for one version would make a matrix job's result
		// ambiguous, and two entries for one image would run the same
		// job twice under different claims.
		if seenVersion[v.EngineVersion] {
			return fmt.Errorf("adapter %s: engine version %q is listed twice", dirName, v.EngineVersion)
		}
		if seenImage[v.Image] {
			return fmt.Errorf("adapter %s: image %q is listed twice", dirName, v.Image)
		}
		seenVersion[v.EngineVersion], seenImage[v.Image] = true, true
		if v.Baseline {
			baselines++
		}
	}
	// Exactly one baseline: none would leave every run with nothing to
	// restore from, and several would make "what the last push proved"
	// depend on manifest order.
	if baselines != 1 {
		return fmt.Errorf("adapter %s: %d verified entries marked baseline, want exactly 1", dirName, baselines)
	}
	return nil
}
