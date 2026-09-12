// Command enginetable rewrites the manifest-derived blocks of the READMEs
// from docs/capabilities.json: the engine table in README.md, and the row
// of engine badges in README.md and every translation of it.
//
// It reads the committed manifest rather than rebuilding the facts from
// the code registries, which makes this repository's own README a consumer
// of the manifest on exactly the terms interfaces set for every other
// consumer: it can state an engine, a version, a release or a source kind
// only because the manifest carries it. The generator that produces the
// manifest runs first, from the //go:generate directive above this one.
//
// The badge row is written into every language because it states nothing
// in any language: engine names and links, which a translation carries
// unchanged. A translation that fell behind would show a shorter list of
// engines than the English README, and nobody would be able to tell
// whether that was a translation lag or a capability difference.
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
	"regexp"
	"sort"
	"strconv"
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
	// badgeStartMarker and badgeEndMarker delimit the engine badge row,
	// which lives in every README rather than only the English one.
	badgeStartMarker = "<!-- capabilities:engine-badges:start -->"
	badgeEndMarker   = "<!-- capabilities:engine-badges:end -->"
	// badgeEndpoint renders the text handed to it and reads nothing, so a
	// badge here cannot report another project's state — the property
	// internal/docs holds every badge in these files to.
	badgeEndpoint = "https://img.shields.io/badge/"
	// neutralColour is the background for an engine this file has no
	// brand colour for. It is nobody's brand: an engine without an icon
	// should read as deliberate rather than as a badge that failed to
	// load, and borrowing Probavi's own colours for somebody else's engine
	// would say something neither project agreed to.
	neutralColour = "4B5563"
	// lightLogo and darkLogo are the two logo colours, chosen per badge by
	// brightness. shields.io picks the *text* colour that way itself; the
	// logo it draws in whatever colour it is handed, so a white icon on
	// ClickHouse yellow simply disappears.
	lightLogo = "white"
	darkLogo  = "333333"
	// logoBrightness is the threshold shields.io uses for its own text
	// colour, so the logo and the words beside it always agree.
	logoBrightness = 0.69
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

// rewrite replaces the marked blocks of the READMEs with the manifest's
// current contents: the engine table in the English README, the badge row
// in that one and in every translation beside it.
func rewrite(root string) error {
	doc, err := loadManifest(filepath.Join(root, filepath.FromSlash(capabilities.Path)))
	if err != nil {
		return err
	}
	if err := rewriteBlock(root, readmeFile, startMarker, endMarker, renderTable(doc.Adapters)); err != nil {
		return err
	}
	files, err := readmes(root)
	if err != nil {
		return err
	}
	badges := renderBadges(doc.Adapters)
	for _, name := range files {
		if err := rewriteBlock(root, name, badgeStartMarker, badgeEndMarker, badges); err != nil {
			return err
		}
	}
	return nil
}

// readmes lists the English README and every translation of it, in a
// stable order. A translation is found rather than listed: a language
// added without touching this file still gets the block, and the missing
// marker it starts life with is an error here rather than a README that
// silently states fewer engines than its source.
func readmes(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", root, err)
	}
	files := []string{readmeFile}
	for _, e := range entries {
		if !e.IsDir() && translationRe.MatchString(e.Name()) {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	return files, nil
}

// rewriteBlock replaces one marked block of one document, writing only
// when the rendering actually differs.
func rewriteBlock(root, name, start, end, body string) error {
	path := filepath.Join(root, name)
	raw, err := os.ReadFile(path) //#nosec G304 -- a repository path assembled from the -root flag.
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	updated, err := replaceBlock(string(raw), body, start, end)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if updated == string(raw) {
		return nil
	}
	// The committed file keeps whatever mode git gave it; the restrictive
	// mode here only applies if it is being created for the first time.
	//#nosec G703 -- the path the read above uses, assembled from the -root flag of a go:generate tool.
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
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
func replaceBlock(doc, body, startMarker, endMarker string) (string, error) {
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

// translationRe matches a translation of the README, the same shape
// docs/i18n.md gives them: the source name with a two-letter locale tag
// before the extension.
var translationRe = regexp.MustCompile(`^README\.[a-z]{2}\.md$`)

// badgeStyle is how one engine is drawn: the simple-icons slug shields.io
// resolves its logo from, and that brand's own colour.
type badgeStyle struct{ logo, colour string }

// engineBadgeStyles is presentation, which is why it lives here and not in
// docs/capabilities.json: a logo slug and a brand colour are facts about
// somebody else's trademark, not claims about what this repository
// restores, and the manifest states only the latter.
//
// Both fields come from simple-icons, the catalogue shields.io itself
// resolves `?logo=` against — so the slug is guaranteed to render and the
// colour is the one that catalogue records for the brand, rather than one
// picked by eye. Eight engines have no entry there, several because the
// brand asked for its icon to be removed; they are listed with an empty
// style rather than left out, so that "no logo" reads as a decision and a
// new adapter still cannot arrive without one. The gate is in
// main_test.go, against the committed manifest.
var engineBadgeStyles = map[string]badgeStyle{
	"aerospike":       {},
	"cassandra":       {logo: "apachecassandra", colour: "1287B1"},
	"clickhouse":      {logo: "clickhouse", colour: "FFCC01"},
	"couchdb":         {logo: "apachecouchdb", colour: "E42528"},
	"duckdb":          {logo: "duckdb", colour: "FFF000"},
	"elasticsearch":   {logo: "elasticsearch", colour: "005571"},
	"etcd":            {logo: "etcd", colour: "419EDA"},
	"firebird":        {},
	"h2":              {logo: "h2database", colour: "09476B"},
	"influxdb":        {logo: "influxdb", colour: "22ADF6"},
	"mariadb":         {logo: "mariadb", colour: "003545"},
	"mongodb":         {logo: "mongodb", colour: "47A248"},
	"mssql":           {},
	"mysql":           {logo: "mysql", colour: "4479A1"},
	"neo4j":           {logo: "neo4j", colour: "4581C3"},
	"opensearch":      {logo: "opensearch", colour: "005EB8"},
	"oracle":          {},
	"postgres":        {logo: "postgresql", colour: "4169E1"},
	"prometheus":      {logo: "prometheus", colour: "E6522C"},
	"qdrant":          {logo: "qdrant", colour: "DC244C"},
	"questdb":         {},
	"redis":           {logo: "redis", colour: "FF4438"},
	"solr":            {logo: "apachesolr", colour: "D9411E"},
	"sqlite":          {logo: "sqlite", colour: "003B57"},
	"tdengine":        {},
	"valkey":          {},
	"victoriametrics": {logo: "victoriametrics", colour: "621773"},
	"weaviate":        {},
}

// renderBadges renders one linked badge per adapter, each carrying the
// engine's name and pointing at the adapter that restores it.
//
// Order is the name a reader sees, not the manifest's id order the table
// below uses. The two blocks answer different questions: the table is read
// downwards, row by row, while the badge row is scanned for one name — and
// a name is looked up where its own first letter puts it, which is why
// "SQL Server" belongs under S rather than where `mssql` would file it.
// Neither order ranks anything; breadth is stated, never celebrated.
func renderBadges(adapters []capabilities.Adapter) string {
	sorted := make([]capabilities.Adapter, len(adapters))
	copy(sorted, adapters)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := strings.ToLower(sorted[i].Name), strings.ToLower(sorted[j].Name)
		if a == b {
			return sorted[i].ID < sorted[j].ID
		}
		return a < b
	})
	b := &strings.Builder{}
	for _, a := range sorted {
		fmt.Fprintf(b, "[![%s](%s)](%s)\n", a.Name, badgeImage(a), adapterLink(a))
	}
	return b.String()
}

// badgeImage builds one shields.io static badge: the engine's name on its
// brand colour, with its logo where one exists.
func badgeImage(a capabilities.Adapter) string {
	style := engineBadgeStyles[a.ID]
	colour := style.colour
	if colour == "" {
		colour = neutralColour
	}
	url := badgeEndpoint + badgeText(a.Name) + "-" + colour
	if style.logo == "" {
		return url
	}
	return url + "?logo=" + style.logo + "&logoColor=" + logoColour(colour)
}

// logoColour picks the logo colour a background can actually show. It is
// shields.io's own brightness rule, so the logo and the name beside it are
// never drawn in opposite colours: white on ClickHouse yellow vanishes,
// and dark on SQLite navy does the same.
func logoColour(hex string) string {
	if brightness(hex) > logoBrightness {
		return darkLogo
	}
	return lightLogo
}

// brightness is the perceived brightness of a six-digit hex colour, 0 to
// 1, by the weights shields.io uses. An unreadable value comes back 0,
// which only means a light logo — a badge nobody can see is worse than a
// badge drawn conservatively.
func brightness(hex string) float64 {
	if len(hex) != 6 {
		return 0
	}
	var rgb [3]float64
	for i := range rgb {
		v, err := strconv.ParseUint(hex[i*2:i*2+2], 16, 8)
		if err != nil {
			return 0
		}
		rgb[i] = float64(v)
	}
	return (rgb[0]*299 + rgb[1]*587 + rgb[2]*114) / 1000 / 255
}

// badgeText escapes a name for shields.io's static endpoint, which reads
// the path it is given as label-message-colour: a dash inside the text
// would split it, and an underscore would come back as a space.
func badgeText(name string) string {
	r := strings.NewReplacer("-", "--", "_", "__", " ", "%20")
	return r.Replace(name)
}

// adapterLink is where a badge sends the reader: the adapter's own
// document when the manifest names one, and its directory otherwise —
// which exists for every declared adapter, because the release builds one
// binary per directory and internal/docs holds the two lists together.
func adapterLink(a capabilities.Adapter) string {
	if a.Docs != nil && *a.Docs != "" {
		return *a.Docs
	}
	return "adapters/" + a.ID + "/"
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
