package capabilities_test

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/probavi/probavi/internal/adapter"
	"github.com/probavi/probavi/internal/capabilities"
	"github.com/probavi/probavi/internal/cli"
	"github.com/probavi/probavi/internal/config"
	"github.com/probavi/probavi/internal/evidence"
	"github.com/probavi/probavi/internal/i18n"
	"github.com/probavi/probavi/internal/notify"
	"github.com/probavi/probavi/internal/sandbox/registry"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files")

// repoRoot is the repository this package describes.
const repoRoot = "../.."

func build(t *testing.T) *capabilities.Document {
	t.Helper()
	doc, err := capabilities.Build(repoRoot)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return doc
}

func render(t *testing.T, doc *capabilities.Document) []byte {
	t.Helper()
	out, err := capabilities.Render(doc)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return out
}

// TestCommittedManifestIsCurrent is the drift gate in test form: the file
// in the tree must be exactly what the code registries produce right now.
// It runs on every `go test ./...`, so a stale manifest is caught before
// CI, and the CI job exists so the failure is also named in a PR.
func TestCommittedManifestIsCurrent(t *testing.T) {
	got := render(t, build(t))
	golden := filepath.Join(repoRoot, filepath.FromSlash(capabilities.Path))
	if *updateGolden {
		if err := os.WriteFile(golden, got, 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read %s (run `go generate ./...`): %v", capabilities.Path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s is stale — run `go generate ./...` and commit the result", capabilities.Path)
	}
}

// TestRenderIsDeterministic pins the property the whole gate rests on: the
// same repository renders the same bytes, so a diff always means a
// capability changed and never that the clock moved.
func TestRenderIsDeterministic(t *testing.T) {
	first := render(t, build(t))
	second := render(t, build(t))
	if !bytes.Equal(first, second) {
		t.Fatal("two builds of the same repository rendered different bytes")
	}
}

// TestRenderedShape covers the contract properties a consumer relies on
// (docs/capabilities.md §1): the generated marker, the versioned format
// id, indentation, and a trailing newline.
func TestRenderedShape(t *testing.T) {
	out := render(t, build(t))
	switch {
	case !bytes.HasSuffix(out, []byte("}\n")):
		t.Error("rendered document does not end with a newline")
	case !bytes.Contains(out, []byte(`"schema": "`+capabilities.SchemaID+`"`)):
		t.Errorf("rendered document does not declare %s", capabilities.SchemaID)
	case !bytes.Contains(out, []byte(`"_generated": "GENERATED FILE`)):
		t.Error("rendered document carries no generated marker")
	case !bytes.Contains(out, []byte("\n  \"project\": {")):
		t.Error("rendered document is not indented with two spaces")
	// The key must survive rendering unescaped, or a consumer reading
	// sandbox parameter ids sees <NAME>.
	case !bytes.Contains(out, []byte(`"env.<NAME>"`)):
		t.Error("rendered document escapes angle brackets")
	}
}

// TestNoVolatileFields pins the "no timestamp, no build metadata" rule
// from the other side: the rendered bytes must not contain the binary
// version or anything clock-shaped, because a field that changes on every
// commit trains everyone to ignore the drift gate.
func TestNoVolatileFields(t *testing.T) {
	out := string(render(t, build(t)))
	for _, key := range []string{
		`"generated_at"`, `"timestamp"`, `"built_at"`, `"date"`,
		`"commit"`, `"revision"`, `"build"`, `"probavi_version"`,
	} {
		if strings.Contains(out, key) {
			t.Errorf("rendered document carries the volatile key %s", key)
		}
	}
	// The binary's version is deliberately absent: it changes per build,
	// while the manifest describes the repository. Matched by shape, not
	// by the literal of the day, so the guard survives a version bump.
	if v := devVersionPattern.FindString(out); v != "" {
		t.Errorf("rendered document carries the binary version: %q", v)
	}
	if when := timestampPattern.FindString(out); when != "" {
		t.Errorf("rendered document carries a timestamp: %q", when)
	}
}

// timestampPattern matches an RFC 3339 instant, the shape any accidental
// clock reading would take. It cannot match an image tag such as
// "2022-latest".
var timestampPattern = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}`)

// devVersionPattern matches the binary's development version stamp
// (`0.3.1-dev`), whatever release it currently trails.
var devVersionPattern = regexp.MustCompile(`\d+\.\d+\.\d+-dev`)

// TestAdaptersTrackTheRepository proves the adapter list is discovered,
// not declared: every directory under adapters/ that carries a probe
// golden appears, so adding an adapter necessarily changes the document.
func TestAdaptersTrackTheRepository(t *testing.T) {
	dirs, err := capabilities.AdapterDirs(repoRoot)
	if err != nil {
		t.Fatalf("AdapterDirs: %v", err)
	}
	want := make([]string, 0, len(dirs))
	for _, d := range dirs {
		want = append(want, filepath.Base(d))
	}
	got := make([]string, 0, len(want))
	for _, a := range build(t).Adapters {
		got = append(got, a.ID)
	}
	assertSameOrder(t, "adapters", got, want)
}

// TestAdaptersMatchTheirProbe pins each adapter entry to the golden its own
// test writes from the live probe payload — the fact that makes the
// adapter section derived rather than declared.
func TestAdaptersMatchTheirProbe(t *testing.T) {
	for _, a := range build(t).Adapters {
		golden, err := os.ReadFile(filepath.Join(
			repoRoot, "adapters", a.ID, "testdata", "probe_response.golden"))
		if err != nil {
			t.Fatalf("read probe golden for %s: %v", a.ID, err)
		}
		probe := string(golden)
		if !strings.Contains(probe, `"adapter_version":"`+a.AdapterVersion+`"`) {
			t.Errorf("%s: adapter_version %q is not what the probe declares", a.ID, a.AdapterVersion)
		}
		if !strings.Contains(probe, `"engine":{"name":"`+a.Engine.ID+`"}`) {
			t.Errorf("%s: engine id %q is not what the probe declares", a.ID, a.Engine.ID)
		}
		for _, s := range a.Sources {
			if !strings.Contains(probe, `"kind":"`+s.ID+`"`) {
				t.Errorf("%s: source kind %q is not what the probe declares", a.ID, s.ID)
			}
		}
	}
}

// TestLocalesTrackEmbeddedCatalogs proves adding a locale catalog changes
// the document: the list is read from the embedded directory, never from a
// list someone must remember to extend.
func TestLocalesTrackEmbeddedCatalogs(t *testing.T) {
	want, err := i18n.Available()
	if err != nil {
		t.Fatalf("i18n.Available: %v", err)
	}
	doc := build(t)
	assertSameOrder(t, "locales", doc.Locales.Available, want)
	if doc.Locales.Source != i18n.SourceLocale {
		t.Errorf("source locale %q, want %q", doc.Locales.Source, i18n.SourceLocale)
	}
	if doc.Locales.Scope != i18n.TranslationScope {
		t.Error("translation scope is not the one internal/i18n declares")
	}
}

// TestChecksTrackTheRegistry proves adding a built-in check changes the
// document.
func TestChecksTrackTheRegistry(t *testing.T) {
	kinds := config.CheckKinds()
	want := make([]string, 0, len(kinds))
	for _, k := range kinds {
		want = append(want, k.ID)
	}
	got := make([]string, 0, len(want))
	for _, c := range build(t).Checks {
		got = append(got, c.ID)
	}
	assertSameOrder(t, "checks", got, want)
}

// TestProvidersTrackTheRegistry proves adding a sandbox provider changes
// the document, parameters included.
func TestProvidersTrackTheRegistry(t *testing.T) {
	descriptors := registry.Descriptors()
	doc := build(t)
	if len(doc.SandboxProviders) != len(descriptors) {
		t.Fatalf("document lists %d providers, registry has %d",
			len(doc.SandboxProviders), len(descriptors))
	}
	for i, d := range descriptors {
		got := doc.SandboxProviders[i]
		if got.ID != d.ID {
			t.Errorf("provider %d: id %q, want %q", i, got.ID, d.ID)
		}
		wantKeys := d.ParamKeys()
		gotKeys := make([]string, 0, len(got.Params))
		for _, p := range got.Params {
			gotKeys = append(gotKeys, p.ID)
		}
		assertSameOrder(t, "provider "+d.ID+" params", gotKeys, wantKeys)
	}
}

// TestCommandsTrackTheTable proves adding a CLI command or an exit code
// changes the document.
func TestCommandsTrackTheTable(t *testing.T) {
	table := cli.Commands()
	doc := build(t)
	if len(doc.CLI.Commands) != len(table) {
		t.Fatalf("document lists %d commands, table has %d", len(doc.CLI.Commands), len(table))
	}
	for i, c := range table {
		got := doc.CLI.Commands[i]
		if got.ID != c.ID {
			t.Errorf("command %d: id %q, want %q", i, got.ID, c.ID)
		}
		if len(got.ExitCodes) != len(c.ExitCodes) {
			t.Errorf("command %s: %d exit codes, want %d", c.ID, len(got.ExitCodes), len(c.ExitCodes))
		}
	}
}

// TestContractVersionsTrackTheConstants pins the published contract
// versions to the constants the binary actually speaks — the answer to an
// auditor asking "written under which schema".
func TestContractVersionsTrackTheConstants(t *testing.T) {
	c := build(t).Contracts
	if c.AdapterProtocol.Version != adapter.ProtocolVersion {
		t.Errorf("adapter protocol %q, want %q", c.AdapterProtocol.Version, adapter.ProtocolVersion)
	}
	if c.EvidenceSchema.Version != evidence.SchemaID {
		t.Errorf("evidence schema %q, want %q", c.EvidenceSchema.Version, evidence.SchemaID)
	}
	assertSameOrder(t, "readable evidence versions", c.EvidenceSchema.ReadableVersions, evidence.SchemaIDs())
	if c.NotificationPayload.Version != notify.SchemaID {
		t.Errorf("notification payload %q, want %q", c.NotificationPayload.Version, notify.SchemaID)
	}
}

// TestNotificationsTrackTheTransports pins the notification section to the
// constants deliveries actually use.
func TestNotificationsTrackTheTransports(t *testing.T) {
	doc := build(t)
	transports := notify.Transports()
	if len(doc.Notifications.Transports) != len(transports) {
		t.Fatalf("document lists %d transports, notify ships %d",
			len(doc.Notifications.Transports), len(transports))
	}
	got := doc.Notifications.Transports[0]
	if got.EventHeader != notify.HeaderEvent || got.Signing.Header != notify.HeaderSignature {
		t.Error("transport headers are not the ones notify sets")
	}
	if got.Delivery.Attempts != notify.Attempts {
		t.Errorf("attempts %d, want %d", got.Delivery.Attempts, notify.Attempts)
	}
	assertSameOrder(t, "outcome filter", got.OutcomeFilter, config.NotifyOutcomes())
}

// TestNonGoalsAreDocumented pins every declared non-goal to the consumer
// contract, so neither list can gain or lose an entry alone. Non-goals are
// the one part of the document no code can derive, which makes this the
// only thing keeping them honest.
func TestNonGoalsAreDocumented(t *testing.T) {
	contract, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(capabilities.ContractDoc)))
	if err != nil {
		t.Fatalf("read contract doc: %v", err)
	}
	doc := build(t)
	if len(doc.NonGoals) == 0 {
		t.Fatal("the document declares no non-goals")
	}
	for _, n := range doc.NonGoals {
		if !bytes.Contains(contract, []byte("`"+n.ID+"`")) {
			t.Errorf("non-goal %q is not listed in %s", n.ID, capabilities.ContractDoc)
		}
		if n.Statement == "" {
			t.Errorf("non-goal %q has no statement", n.ID)
		}
	}
}

// TestGenerateWritesTheDocument covers the path the CI gate runs.
func TestGenerateWritesTheDocument(t *testing.T) {
	out := filepath.Join(t.TempDir(), "capabilities.json")
	if err := capabilities.Generate(repoRoot, out); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	written, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read generated file: %v", err)
	}
	if !bytes.Equal(written, render(t, build(t))) {
		t.Error("Generate wrote something other than the rendered document")
	}
}

// TestGenerateReportsFailures covers the two ways generation fails: an
// unreadable repository and an unwritable destination.
func TestGenerateReportsFailures(t *testing.T) {
	if err := capabilities.Generate(filepath.Join(t.TempDir(), "absent"), "out.json"); err == nil {
		t.Error("Generate accepted a repository root that does not exist")
	}
	dir := t.TempDir()
	if err := capabilities.Generate(repoRoot, filepath.Join(dir, "no-such-dir", "out.json")); err == nil {
		t.Error("Generate accepted an unwritable destination")
	}
}

func assertSameOrder(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d entries %v, want %d %v", what, len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s[%d] = %q, want %q", what, i, got[i], want[i])
		}
	}
}

// TestNoVerifiedVersionIsPastItsSupport is the mechanism behind
// engine-versions.md §1 rule 1, which until now was a rule with nobody to
// enforce it: a version past end-of-life must not be listed, and the only
// thing that noticed was a human remembering to look.
//
// It reads the calendar, so it can fail on a day nobody touched the
// repository. That is the point. A claim that this project verifies
// restores from an engine version its own vendor has stopped supporting
// goes stale on a date, not on a commit, and the build is the only place
// that sees every date at once. The fix is always the same: drop the
// entry, or move to a version the vendor still supports.
//
// Versions whose vendor publishes no end date carry null and are skipped;
// versions_checked in the same manifest records when a human last read
// that vendor's page, which is what makes the null a statement.
func TestNoVerifiedVersionIsPastItsSupport(t *testing.T) {
	dirs, err := capabilities.AdapterDirs(repoRoot)
	if err != nil {
		t.Fatalf("list adapters: %v", err)
	}
	if len(dirs) == 0 {
		t.Fatal("no adapters found — this gate would pass vacuously")
	}
	today := time.Now()
	for _, dir := range dirs {
		m, merr := capabilities.LoadAdapterManifest(dir)
		if merr != nil {
			t.Errorf("%s: %v", dir, merr)
			continue
		}
		for _, v := range m.Verified {
			if v.SupportedUntil == nil {
				continue
			}
			closed, cerr := capabilities.SupportWindowClosed(*v.SupportedUntil, today)
			if cerr != nil {
				t.Errorf("%s %s: %v", m.ID, v.EngineVersion, cerr)
				continue
			}
			if closed {
				t.Errorf("%s claims %s, whose vendor support ended on %s — remove the entry or "+
					"replace it with a supported version (docs/engine-versions.md §1)",
					m.ID, v.EngineVersion, *v.SupportedUntil)
			}
		}
	}
}
