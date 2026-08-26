// Command enginetable rewrites the engine table in README.md from
// docs/capabilities.json.
//
// It reads the committed manifest rather than rebuilding the facts from
// the code registries, which makes this repository's own README a consumer
// of the manifest on exactly the terms interfaces set for every other
// consumer: it can state an engine, a version, a release or a source kind
// only because the manifest carries it. The generator that produces the
// manifest runs first, from the //go:generate directive above this one.
//
// Run it through `go generate ./...`. CI regenerates and fails on any
// difference (AGENTS.md §5.8).
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

const (
	// readmeFile is the document the block lives in.
	readmeFile = "README.md"
	// startMarker and endMarker delimit the generated block. They are HTML
	// comments so they render as nothing and survive every markdown tool.
	startMarker = "<!-- capabilities:engines:start -->"
	endMarker   = "<!-- capabilities:engines:end -->"
	// unreleased is what the release column says for an adapter that is in
	// the tree but has not been in a tagged release yet. The manifest
	// carries null; a blank cell would read as an omission.
	unreleased = "unreleased"
)

func main() {
	if err := run(os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "enginetable: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("enginetable", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", ".", "repository root to rewrite the table in")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return rewrite(*root)
}

// rewrite replaces the marked block of the README with the current table.
func rewrite(root string) error {
	doc, err := loadManifest(filepath.Join(root, filepath.FromSlash(capabilities.Path)))
	if err != nil {
		return err
	}
	path := filepath.Join(root, readmeFile)
	raw, err := os.ReadFile(path) //#nosec G304 -- a repository path assembled from the -root flag.
	if err != nil {
		return fmt.Errorf("read %s: %w", readmeFile, err)
	}
	updated, err := replaceBlock(string(raw), renderTable(doc.Adapters))
	if err != nil {
		return fmt.Errorf("%s: %w", readmeFile, err)
	}
	if updated == string(raw) {
		return nil
	}
	// The committed file keeps whatever mode git gave it; the restrictive
	// mode here only applies if it is being created for the first time.
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", readmeFile, err)
	}
	return nil
}

// loadManifest reads docs/capabilities.json the way a consumer must:
// refusing a schema version it does not recognise rather than guessing at
// the fields (docs/capabilities.md §2).
func loadManifest(path string) (*capabilities.Document, error) {
	raw, err := os.ReadFile(path) //#nosec G304 -- a repository path assembled from the -root flag.
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", capabilities.Path, err)
	}
	doc := &capabilities.Document{}
	if err := json.Unmarshal(raw, doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", capabilities.Path, err)
	}
	if doc.SchemaID != capabilities.SchemaID {
		return nil, fmt.Errorf("%s declares schema %q, this tool reads %q",
			capabilities.Path, doc.SchemaID, capabilities.SchemaID)
	}
	if len(doc.Adapters) == 0 {
		return nil, fmt.Errorf("%s lists no adapters", capabilities.Path)
	}
	return doc, nil
}

// replaceBlock swaps the content between the markers for body. A missing,
// duplicated or inverted pair is an error: silently appending the table
// somewhere, or writing it twice, would publish a claim nobody reviewed.
func replaceBlock(doc, body string) (string, error) {
	start, err := marker(doc, startMarker)
	if err != nil {
		return "", err
	}
	end, err := marker(doc, endMarker)
	if err != nil {
		return "", err
	}
	if end < start {
		return "", fmt.Errorf("%s appears before %s", endMarker, startMarker)
	}
	return doc[:start+len(startMarker)] + "\n" + body + doc[end:], nil
}

// marker returns the single offset of want, failing when it is absent or
// repeated.
func marker(doc, want string) (int, error) {
	i := strings.Index(doc, want)
	if i < 0 {
		return 0, fmt.Errorf("no %s marker", want)
	}
	if strings.Contains(doc[i+len(want):], want) {
		return 0, fmt.Errorf("%s appears more than once", want)
	}
	return i, nil
}

// renderTable renders the engine table. Row order is the manifest's own — the
// adapters sorted by id — and never a popularity order: the list is stated,
// not celebrated.
func renderTable(adapters []capabilities.Adapter) string {
	b := &strings.Builder{}
	b.WriteString("| Engine | Verified against | In every release since | Source kinds |\n")
	b.WriteString("| --- | --- | --- | --- |\n")
	for _, a := range adapters {
		fmt.Fprintf(b, "| %s | %s | %s | %s |\n",
			engineCell(a), versionCell(a), sinceCell(a), sourcesCell(a))
	}
	return b.String()
}

// engineCell names the engine and links to the adapter that restores it.
func engineCell(a capabilities.Adapter) string {
	if a.Docs == nil || *a.Docs == "" {
		return a.Name
	}
	return fmt.Sprintf("[%s](%s)", a.Name, *a.Docs)
}

// versionCell lists the engine versions CI restores from — a record of
// what was tested, never a supported-version range (docs/capabilities.md
// §1.3).
//
// When an adapter's entries come from more than one image repository, some
// of them are variants: the same engine shipped differently, such as
// Percona Server, or PostgreSQL with pgvector bundled in. A bare version
// column would read those as engine versions of the engine named in the
// first column, so a version is qualified by the short name of the
// repository its image came from.
//
// Which entries need qualifying is decided by counting, not by judging
// which repository is the "real" one: the repository that holds strictly
// more entries than any other stands unqualified, and everything else is
// named. Where no repository holds more than the rest — an adapter with
// one plain image and one variant — every version is named, which is the
// honest answer to a question the manifest does not settle.
func versionCell(a capabilities.Adapter) string {
	counts := make(map[string]int, len(a.Verified))
	for _, v := range a.Verified {
		counts[imageRepo(v.Image)]++
	}
	plain := majorityRepo(counts)
	parts := make([]string, 0, len(a.Verified))
	for _, v := range a.Verified {
		repo := imageRepo(v.Image)
		if repo == plain {
			parts = append(parts, v.EngineVersion)
			continue
		}
		parts = append(parts, shortRepo(repo)+" "+v.EngineVersion)
	}
	return strings.Join(parts, ", ")
}

// majorityRepo returns the repository holding strictly more entries than
// any other, or "" when there is no such repository. Order in, order out:
// it reads a count, so a reordered manifest cannot change the answer.
func majorityRepo(counts map[string]int) string {
	best, bestCount, tied := "", 0, false
	for repo, n := range counts {
		switch {
		case n > bestCount:
			best, bestCount, tied = repo, n, false
		case n == bestCount:
			tied = true
		}
	}
	if tied {
		return ""
	}
	return best
}

// imageRepo drops an image reference's tag. A digest-pinned reference
// keeps its digest, which still identifies the repository uniquely.
func imageRepo(image string) string {
	i := strings.LastIndex(image, ":")
	slash := strings.LastIndex(image, "/")
	if i > slash {
		return image[:i]
	}
	return image
}

// shortRepo is the last path element of a repository — "percona-server"
// out of "percona/percona-server", "postgres" out of "postgres".
func shortRepo(repo string) string {
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		return repo[i+1:]
	}
	return repo
}

// sinceCell states the release the adapter first shipped in.
func sinceCell(a capabilities.Adapter) string {
	if a.Since == nil || *a.Since == "" {
		return unreleased
	}
	return *a.Since
}

// sourcesCell lists the source kinds by the id a drill config writes.
func sourcesCell(a capabilities.Adapter) string {
	parts := make([]string, 0, len(a.Sources))
	for _, s := range a.Sources {
		parts = append(parts, "`"+s.ID+"`")
	}
	return strings.Join(parts, ", ")
}
