package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/probavi/probavi/internal/capabilities"
)

// main_test.go holds the tool to the one thing it must never do: publish a
// cell the manifest does not carry. Every case here is a way the README
// could end up saying more than docs/capabilities.json does.

func ptr(s string) *string { return &s }

// blocks is a document carrying both generated regions, which is what every
// README in the repository looks like to this tool.
const blocks = startMarker + "\n" + endMarker + "\n\n" + badgeStartMarker + "\n" + badgeEndMarker + "\n"

// demo is the adapter shape the cases start from: one image repository,
// one released version, two source kinds.
func demo() capabilities.Adapter {
	return capabilities.Adapter{
		ID:     "demo",
		Name:   "Demo Engine",
		Status: capabilities.StatusExperimental,
		Since:  ptr("0.4.0"),
		Verified: []capabilities.VerifiedTarget{
			{EngineVersion: "16", Image: "demo:16"},
		},
		Sources: []capabilities.Source{{ID: "demo_dump"}, {ID: "demo_dump_dir"}},
		Docs:    ptr("adapters/demo/README.md"),
	}
}

func TestRenderTableRowsCarryEveryColumn(t *testing.T) {
	got := renderTable([]capabilities.Adapter{demo()})
	want := "| Engine | Verified against | In every release since | Source kinds |\n" +
		"| --- | --- | --- | --- |\n" +
		"| [Demo Engine](adapters/demo/README.md) | 16 | 0.4.0 | `demo_dump`, `demo_dump_dir` |\n"
	if got != want {
		t.Errorf("table =\n%s\nwant\n%s", got, want)
	}
}

func TestRenderTableKeepsManifestOrder(t *testing.T) {
	// The manifest sorts adapters by id, and that order is what ships: a
	// table sorted by popularity would read as a scoreboard, which is not
	// what a list of what CI restores from is.
	first, second := demo(), demo()
	first.ID, first.Name = "zulu", "Zulu"
	second.ID, second.Name = "alpha", "Alpha"
	got := renderTable([]capabilities.Adapter{first, second})
	if strings.Index(got, "Zulu") > strings.Index(got, "Alpha") {
		t.Errorf("table reordered its input:\n%s", got)
	}
}

func TestVersionCell(t *testing.T) {
	tests := map[string]struct {
		verified []capabilities.VerifiedTarget
		want     string
	}{
		"one repository stands unqualified": {
			[]capabilities.VerifiedTarget{
				{EngineVersion: "14", Image: "postgres:14"},
				{EngineVersion: "15", Image: "postgres:15"},
			},
			"14, 15",
		},
		// The variants are what a bare version column would misreport: a
		// reader would take "0.8.6-pg17" for a PostgreSQL version.
		"a minority repository is named": {
			[]capabilities.VerifiedTarget{
				{EngineVersion: "16", Image: "postgres:16"},
				{EngineVersion: "17", Image: "postgres:17"},
				{EngineVersion: "0.8.6-pg17", Image: "pgvector/pgvector:0.8.6-pg17"},
			},
			"16, 17, pgvector 0.8.6-pg17",
		},
		// One plain image and one variant: nothing is in the majority, so
		// the manifest does not settle which is which, and neither does the
		// table.
		"a tie names everything": {
			[]capabilities.VerifiedTarget{
				{EngineVersion: "8.4", Image: "mysql:8.4"},
				{EngineVersion: "8.4.10", Image: "percona/percona-server:8.4.10"},
			},
			"mysql 8.4, percona-server 8.4.10",
		},
		"a registry host is not mistaken for a tag": {
			[]capabilities.VerifiedTarget{
				{EngineVersion: "3.5", Image: "quay.io/coreos/etcd:v3.5.0"},
				{EngineVersion: "3.6", Image: "quay.io/coreos/etcd:v3.6.0"},
			},
			"3.5, 3.6",
		},
		"a port in a registry host is not a tag": {
			[]capabilities.VerifiedTarget{
				{EngineVersion: "1", Image: "localhost:5000/demo"},
			},
			"1",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			a := demo()
			a.Verified = tt.verified
			if got := versionCell(a); got != tt.want {
				t.Errorf("versionCell = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMajorityRepoIsIndependentOfOrder(t *testing.T) {
	// Map iteration is randomized, so a rule that reads a count has to
	// give the same answer every time it runs. A generated file whose
	// bytes depend on the run is not a generated file.
	counts := map[string]int{"postgres": 4, "pgvector": 1, "timescaledb": 1}
	for range 50 {
		if got := majorityRepo(counts); got != "postgres" {
			t.Fatalf("majorityRepo = %q, want postgres", got)
		}
	}
	for range 50 {
		if got := majorityRepo(map[string]int{"mysql": 1, "percona": 1}); got != "" {
			t.Fatalf("majorityRepo = %q, want no majority", got)
		}
	}
}

func TestSinceCellSaysUnreleasedRatherThanNothing(t *testing.T) {
	a := demo()
	a.Since = nil
	if got := sinceCell(a); got != unreleased {
		t.Errorf("sinceCell = %q, want %q", got, unreleased)
	}
	empty := ""
	a.Since = &empty
	if got := sinceCell(a); got != unreleased {
		t.Errorf("sinceCell = %q, want %q", got, unreleased)
	}
}

func TestEngineCellSurvivesAnAdapterWithoutDocs(t *testing.T) {
	a := demo()
	a.Docs = nil
	if got := engineCell(a); got != "Demo Engine" {
		t.Errorf("engineCell = %q, want the bare name", got)
	}
}

func TestReplaceBlockRefusesAMalformedDocument(t *testing.T) {
	tests := map[string]struct{ doc, want string }{
		"no start marker":  {endMarker, "no " + startMarker},
		"no end marker":    {startMarker, "no " + endMarker},
		"start repeated":   {startMarker + "\n" + startMarker + "\n" + endMarker, "more than once"},
		"end repeated":     {startMarker + "\n" + endMarker + "\n" + endMarker, "more than once"},
		"markers inverted": {endMarker + "\n" + startMarker, "before"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := replaceBlock(tt.doc, "table\n", startMarker, endMarker)
			if err == nil {
				t.Fatal("replaceBlock accepted a document it cannot mark up")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to carry %q", err, tt.want)
			}
		})
	}
}

func TestReplaceBlockKeepsEverythingOutsideTheMarkers(t *testing.T) {
	doc := "before\n\n" + startMarker + "\nstale\n" + endMarker + "\n\nafter\n"
	got, err := replaceBlock(doc, "fresh\n", startMarker, endMarker)
	if err != nil {
		t.Fatal(err)
	}
	want := "before\n\n" + startMarker + "\nfresh\n" + endMarker + "\n\nafter\n"
	if got != want {
		t.Errorf("replaceBlock =\n%q\nwant\n%q", got, want)
	}
}

// fixture writes a repository root holding a manifest and a README.
func fixture(t *testing.T, doc any, readme string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o750); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(capabilities.Path)), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, readmeFile), []byte(readme), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func validDoc() *capabilities.Document {
	return &capabilities.Document{SchemaID: capabilities.SchemaID, Adapters: []capabilities.Adapter{demo()}}
}

func TestRunRewritesTheBlock(t *testing.T) {
	root := fixture(t, validDoc(), "# Title\n\n"+blocks+"\ntail\n")
	if err := run([]string{"-root", root}, &bytes.Buffer{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, readmeFile))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"# Title", "Demo Engine", "0.4.0", "`demo_dump`", "tail",
		"[![Demo Engine](https://img.shields.io/badge/Demo%20Engine-informational)](adapters/demo/README.md)"}
	for _, want := range want {
		if !strings.Contains(string(got), want) {
			t.Errorf("README lost %q:\n%s", want, got)
		}
	}
	// Running twice must change nothing: the CI gate compares the tree
	// after generation, so a tool that is not idempotent fails every run.
	before := string(got)
	if err := run([]string{"-root", root}, &bytes.Buffer{}); err != nil {
		t.Fatalf("second run: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(root, readmeFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != before {
		t.Errorf("second run changed the file:\n%s", after)
	}
}

func TestRunRefusesAManifestItCannotTrust(t *testing.T) {
	readme := blocks
	stale := validDoc()
	stale.SchemaID = "probavi-capabilities/99"
	empty := validDoc()
	empty.Adapters = nil

	tests := map[string]struct {
		root func(t *testing.T) string
		want string
	}{
		"an unknown schema version": {
			func(t *testing.T) string { return fixture(t, stale, readme) },
			"declares schema",
		},
		"a manifest listing nothing": {
			func(t *testing.T) string { return fixture(t, empty, readme) },
			"lists no adapters",
		},
		"a manifest that is not JSON": {
			func(t *testing.T) string { return fixture(t, "{", readme) },
			"parse",
		},
		"a README without markers": {
			func(t *testing.T) string { return fixture(t, validDoc(), "# Title\n") },
			"no " + startMarker,
		},
		"no repository at all": {
			func(t *testing.T) string { return t.TempDir() },
			"read " + capabilities.Path,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := run([]string{"-root", tt.root(t)}, &bytes.Buffer{})
			if err == nil {
				t.Fatal("run accepted it")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to carry %q", err, tt.want)
			}
		})
	}
}

func TestRunReportsAReadOnlyReadme(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes everything")
	}
	root := fixture(t, validDoc(), blocks)
	path := filepath.Join(root, readmeFile)
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	err := run([]string{"-root", root}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("run reported success without writing anything")
	}
	if !strings.Contains(err.Error(), "write "+readmeFile) {
		t.Errorf("err = %v, want it to name the write it could not do", err)
	}
}

func TestRunRejectsUnknownArguments(t *testing.T) {
	if err := run([]string{"stray"}, &bytes.Buffer{}); err == nil {
		t.Error("run accepted a stray argument")
	}
	if err := run([]string{"-nope"}, &bytes.Buffer{}); err == nil {
		t.Error("run accepted an unknown flag")
	}
}

func TestRenderBadgesOrdersByTheNameOnTheBadge(t *testing.T) {
	// Manifest order is by id, and for three adapters the id files the
	// engine somewhere its name does not: mssql/SQL Server is the clearest.
	// The badge row is scanned for a name, so it sorts by the name.
	sqlServer, aerospike := demo(), demo()
	sqlServer.ID, sqlServer.Name, sqlServer.Docs = "mssql", "SQL Server", ptr("adapters/mssql/README.md")
	aerospike.ID, aerospike.Name, aerospike.Docs = "aerospike", "Aerospike", ptr("adapters/aerospike/README.md")

	got := renderBadges([]capabilities.Adapter{aerospike, sqlServer})
	if strings.Index(got, "Aerospike") > strings.Index(got, "SQL%20Server") {
		t.Errorf("badges are not in name order:\n%s", got)
	}

	// And the other direction: manifest order must not survive when it
	// disagrees with the alphabet.
	got = renderBadges([]capabilities.Adapter{sqlServer, aerospike})
	if strings.Index(got, "Aerospike") > strings.Index(got, "SQL%20Server") {
		t.Errorf("badges kept the manifest's order instead of the alphabet:\n%s", got)
	}
}

func TestRenderBadgesIsCaseInsensitiveAndStable(t *testing.T) {
	// etcd is spelled lowercase on purpose (reference/glossary.md), and a
	// byte-order sort would file every lowercase name after every
	// uppercase one — etcd after Weaviate, at the end of the row.
	etcd, weaviate := demo(), demo()
	etcd.ID, etcd.Name, etcd.Docs = "etcd", "etcd", ptr("adapters/etcd/README.md")
	weaviate.ID, weaviate.Name, weaviate.Docs = "weaviate", "Weaviate", ptr("adapters/weaviate/README.md")

	got := renderBadges([]capabilities.Adapter{weaviate, etcd})
	if strings.Index(got, "etcd") > strings.Index(got, "Weaviate") {
		t.Errorf("lowercase name sorted after every uppercase one:\n%s", got)
	}
}

func TestBadgeTextEscapesTheShieldsSeparators(t *testing.T) {
	// shields.io reads the path as label-message-colour and renders an
	// underscore as a space, so a name carrying either comes out as
	// something else entirely — or splits the badge into two fields.
	tests := map[string]string{
		"SQL Server":      "SQL%20Server",
		"Foo-Bar":         "Foo--Bar",
		"foo_bar":         "foo__bar",
		"PostgreSQL":      "PostgreSQL",
		"Oracle Database": "Oracle%20Database",
	}
	for name, want := range tests {
		if got := badgeText(name); got != want {
			t.Errorf("badgeText(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestAdapterLinkFallsBackToTheDirectory(t *testing.T) {
	// A badge has nowhere to send a reader without a link, and the manifest
	// leaves docs optional. The directory is there for every declared
	// adapter — the release builds one binary per directory.
	a := demo()
	a.Docs = nil
	if got := adapterLink(a); got != "adapters/demo/" {
		t.Errorf("adapterLink = %q, want the adapter's directory", got)
	}
	a.Docs = ptr("")
	if got := adapterLink(a); got != "adapters/demo/" {
		t.Errorf("adapterLink on an empty docs field = %q, want the adapter's directory", got)
	}
}

func TestRunWritesTheBadgeRowIntoEveryTranslation(t *testing.T) {
	root := fixture(t, validDoc(), "# Title\n\n"+blocks)
	for _, name := range []string{"README.hu.md", "README.de.md"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("# Cím\n\n"+badgeStartMarker+"\n"+badgeEndMarker+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Not a README, and not a translation of one: it must be left alone.
	if err := os.WriteFile(filepath.Join(root, "CHANGELOG.md"), []byte("# Changelog\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-root", root}, &bytes.Buffer{}); err != nil {
		t.Fatalf("run: %v", err)
	}

	badge := "[![Demo Engine](https://img.shields.io/badge/Demo%20Engine-informational)](adapters/demo/README.md)"
	for _, name := range []string{readmeFile, "README.hu.md", "README.de.md"} {
		got, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(got), badge) {
			t.Errorf("%s carries no engine badge:\n%s", name, got)
		}
	}
	// The engine table stays English-only: it is prose-headed and the
	// translations do not carry it.
	hu, err := os.ReadFile(filepath.Join(root, "README.hu.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(hu), "| Engine |") {
		t.Errorf("the engine table leaked into a translation:\n%s", hu)
	}
}

func TestRunReportsATranslationWithoutTheBadgeMarkers(t *testing.T) {
	// A language added without the markers must stop the generator rather
	// than quietly ship a README stating fewer engines than its source.
	root := fixture(t, validDoc(), "# Title\n\n"+blocks)
	if err := os.WriteFile(filepath.Join(root, "README.fr.md"), []byte("# Titre\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := run([]string{"-root", root}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("run accepted a translation with no badge block")
	}
	if !strings.Contains(err.Error(), "README.fr.md") || !strings.Contains(err.Error(), badgeStartMarker) {
		t.Errorf("err = %v, want it to name the file and the missing marker", err)
	}
}
