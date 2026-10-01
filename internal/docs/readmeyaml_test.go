package docs_test

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"

	"github.com/probavi/probavi/internal/config"
)

// newcomerDoc is the document this gate holds to the loader: the first
// thing a stranger reads, and the thing they copy out of before they have
// an opinion about whether the project works.
//
// It is spelled out here rather than borrowed from the translation gate's
// sourceDoc, which happens to name the same file for a different reason.
// Were that one ever pointed somewhere else, this gate would follow it to
// a document nobody copies from and keep reporting success.
//
// One document is the whole surface rather than a sample. The translated
// READMEs carry no YAML at all — they are landing pages that point here —
// and examples/drill.example.yaml is already loaded by internal/config's
// own suite, which is why it cannot drift. Everything else a reader can
// copy is in docs/, and that is a wider job than this one (ROADMAP).
const newcomerDoc = "README.md"

// gameDayMemberKey is the top-level key that tells the two document kinds
// apart. A drill configuration has no members; a game-day is defined by
// having them.
const gameDayMemberKey = "members"

var (
	// fenceRe matches any opening code fence, capturing the indentation so
	// the block can be read back at column zero, and the info string so the
	// block can be told what it claims to be. The indentation is not
	// hypothetical: the k8s sandbox example is nested inside a list item,
	// and the first survey of this document missed it for exactly that
	// reason.
	//
	// Every fence is read rather than only the YAML ones, because the fence
	// is a rendering hint and this gate's coverage must not depend on it.
	// Measured: with the matcher keyed on "```yaml" alone, respelling the
	// fence of five of the six blocks removed them from the gate and it
	// still reported success — the surviving block satisfied the
	// found-something guard by itself.
	fenceRe = regexp.MustCompile("^([\t ]*)```([A-Za-z0-9]*)[\t ]*$")
	// yamlInfoRe matches the info strings a YAML block is written with, in
	// any case: a block that says it is YAML must be a document this binary
	// reads, or be classified here.
	yamlInfoRe = regexp.MustCompile(`(?i)^ya?ml$`)
	// topLevelKeyRe matches a mapping key at column zero. Nested keys are
	// indented and comments start with #, so what this finds is the set of
	// sections a block declares.
	topLevelKeyRe = regexp.MustCompile(`(?m)^([A-Za-z_][A-Za-z0-9_]*):`)
)

// skeletonOrder is the order the composed document carries its filled-in
// sections. YAML does not care; a failure message that a human has to read
// does.
var skeletonOrder = []string{"target", "sandbox", "checks", "evidence"}

// drillSkeleton supplies the required sections a fragment leaves out, so a
// block that documents one part of a configuration can still be handed to
// the loader whole.
//
// The texts are the minimum config.Load accepts, which makes the skeleton
// self-checking: if a required field were renamed, every fragment in the
// document would start failing with the loader's own diagnostic rather
// than the skeleton quietly going stale.
var drillSkeleton = map[string]string{
	"target": `target:
  name: skeleton
  adapter: postgres
  source:
    kind: pgdump
    path: backup.dump
`,
	"sandbox": `sandbox:
  provider: docker
  params:
    image: postgres:16
  timeout: 30m
`,
	"checks": `checks:
  - builtin: service_healthy
`,
	"evidence": evidenceSection("evidence.jsonl"),
}

// evidenceSection is the skeleton's evidence section, parameterised by the
// log it names. A game-day that sets max_parallel above 1 refuses two
// members sharing one log, so each materialised member needs its own — and
// taking the name as an argument is what keeps that true: patching the
// path into the text afterwards would quietly stop working the day the
// section is reworded.
func evidenceSection(log string) string {
	return "evidence:\n  path: " + log + "\n  sign_key: probavi.key\n"
}

// fencedBlock is one fenced code block, dedented to column zero, with the
// line of its opening fence kept so a failure can name where to look and
// the fence's info string kept so an unrecognised block can be judged.
type fencedBlock struct {
	line int
	info string
	body string
}

// TestEveryYAMLBlockInTheReadmeIsADocumentTheLoaderAccepts holds the
// README's copy-paste surface to the binary that has to read it.
//
// The configuration loader is strict — unknown fields and duplicate keys
// are errors — so a renamed or removed key breaks every drill file in the
// field at once. It does not break the README, which is prose, and the
// README is where a newcomer's first drill comes from. examples/ is gated
// because internal/config loads it; the blocks here were gated by nobody,
// and the only measurement of them was a maintainer running the quickstart
// by hand in Phase 1.
//
// What a block is gets decided by what it declares, not by how it is
// fenced — so every drill configuration and every game-day in the document
// is loaded, and a block fenced as YAML that is neither of those is a
// failure rather than a quiet pass. A classification that waves through
// what it does not recognise reports as tested whatever the document says,
// which is the shape skiponly_test.go was written for. The blocks that are
// neither and never claimed to be — the console transcripts, the one
// JavaScript example — are left alone.
func TestEveryYAMLBlockInTheReadmeIsADocumentTheLoaderAccepts(t *testing.T) {
	blocks := fencedBlocks(t, newcomerDoc)
	if len(blocks) == 0 {
		t.Fatalf("%s: no fenced code blocks found at all; this gate now measures nothing",
			newcomerDoc)
	}

	sections := drillSections(t)
	drills, gameDays := 0, 0
	for _, b := range blocks {
		keys := topLevelKeys(b.body)
		switch {
		case slices.Contains(keys, gameDayMemberKey):
			loadGameDayBlock(t, b)
			gameDays++
		case subset(keys, sections):
			loadDrillBlock(t, b)
			drills++
		case yamlInfoRe.MatchString(b.info):
			t.Errorf("%s:%d is fenced as %q and declares %v, which names neither a drill "+
				"configuration section (%v) nor a game-day. Classify it here or this gate "+
				"is measuring around it", newcomerDoc, b.line, b.info, keys, sections)
		}
	}

	// The document's whole pitch is a drill as code, and the one above it is
	// a game-day. If neither is in the document any more, the gate has
	// stopped doing the job it was added for and should say so rather than
	// pass on whatever is left.
	if drills == 0 || gameDays == 0 {
		t.Errorf("%s: %d fenced blocks, %d drill configurations and %d game-days — "+
			"the gate expects at least one of each", newcomerDoc, len(blocks), drills, gameDays)
	}
	t.Logf("%s: %d fenced blocks, %d drill configurations, %d game-days",
		newcomerDoc, len(blocks), drills, gameDays)
}

// loadDrillBlock composes the block into a whole drill configuration and
// loads it. The block's own bytes go in verbatim — strictness is the point
// of the gate, and a round-trip through a generic decoder would quietly
// absorb a duplicate key the loader is supposed to refuse.
func loadDrillBlock(t *testing.T, b fencedBlock) {
	t.Helper()

	declared := topLevelKeys(b.body)
	var doc strings.Builder
	doc.WriteString(b.body)
	if !strings.HasSuffix(b.body, "\n") {
		doc.WriteString("\n")
	}
	for _, name := range skeletonOrder {
		if slices.Contains(declared, name) {
			continue
		}
		doc.WriteString(drillSkeleton[name])
	}

	path := filepath.Join(t.TempDir(), "drill.yaml")
	if err := os.WriteFile(path, []byte(doc.String()), 0o600); err != nil {
		t.Fatalf("write composed drill configuration: %v", err)
	}
	if _, err := config.Load(path, nil); err != nil {
		t.Errorf("%s:%d is not a drill configuration the loader accepts:\n%v\n"+
			"--- document as composed ---\n%s", newcomerDoc, b.line, err, doc.String())
	}
}

// loadGameDayBlock loads the block as a game-day, which means materialising
// the member drill files it names: LoadGameDay resolves and loads every
// member before the exercise starts, so a game-day document cannot be
// validated on its own. Each member gets its own evidence path, because a
// game-day that sets max_parallel above 1 refuses two members sharing one
// log — a rule about the document, not something this gate should trip over
// by handing out identical members.
func loadGameDayBlock(t *testing.T, b fencedBlock) {
	t.Helper()

	var declared struct {
		Members []struct {
			Config string `yaml:"config"`
		} `yaml:"members"`
	}
	if err := yaml.Unmarshal([]byte(b.body), &declared); err != nil {
		t.Errorf("%s:%d is not readable as YAML: %v", newcomerDoc, b.line, err)
		return
	}

	dir := t.TempDir()
	for i, m := range declared.Members {
		if m.Config == "" {
			continue
		}
		// filepath.IsLocal rather than !filepath.IsAbs: a member naming
		// "../../etc/x" is relative and would still have this test writing
		// outside its temporary directory, onto the machine running it.
		// Materialising only a contained path is also no loss to the
		// document — the README's own comment says a member "stays runnable
		// from cron" — so the gate states its limit instead of reaching for
		// the real filesystem.
		if !filepath.IsLocal(m.Config) {
			t.Errorf("%s:%d names member[%d] config %q, which is not a path inside the "+
				"game-day's own directory; this gate materialises only contained members",
				newcomerDoc, b.line, i, m.Config)
			return
		}
		path := filepath.Join(dir, m.Config)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("make member directory: %v", err)
		}
		if err := os.WriteFile(path, []byte(skeletonDrill(i)), 0o600); err != nil {
			t.Fatalf("write member drill configuration: %v", err)
		}
	}

	path := filepath.Join(dir, "gameday.yaml")
	if err := os.WriteFile(path, []byte(b.body), 0o600); err != nil {
		t.Fatalf("write game-day configuration: %v", err)
	}
	if _, err := config.LoadGameDay(path, nil); err != nil {
		t.Errorf("%s:%d is not a game-day configuration the loader accepts:\n%v",
			newcomerDoc, b.line, err)
	}
}

// skeletonDrill is a whole minimal drill configuration, with an evidence
// path of its own.
func skeletonDrill(n int) string {
	var doc strings.Builder
	for _, name := range skeletonOrder {
		if name == "evidence" {
			doc.WriteString(evidenceSection("evidence-" + strconv.Itoa(n) + ".jsonl"))
			continue
		}
		doc.WriteString(drillSkeleton[name])
	}
	return doc.String()
}

// drillSections are the top-level keys a drill configuration may carry,
// read off config.Config's own yaml tags rather than listed here: a new
// section arrives with the struct, and a block that uses it classifies
// without this file being touched.
func drillSections(t *testing.T) []string {
	t.Helper()

	var out []string
	typ := reflect.TypeOf(config.Config{})
	for i := range typ.NumField() {
		tag, _, _ := strings.Cut(typ.Field(i).Tag.Get("yaml"), ",")
		if tag == "" || tag == "-" {
			continue
		}
		out = append(out, tag)
	}
	if len(out) == 0 {
		t.Fatalf("config.Config declares no yaml tags; every block would classify as unknown")
	}
	return out
}

// fencedBlocks extracts every fenced code block from a repository
// document, dedented to column zero.
func fencedBlocks(t *testing.T, name string) []fencedBlock {
	t.Helper()

	lines := strings.Split(read(t, name), "\n")
	var out []fencedBlock
	// consumed is the last line already inside a block. Without it the scan
	// re-reads block bodies, and a fence written *inside* one — a document
	// about Markdown, a transcript of this file — would open a second,
	// overlapping block.
	consumed := -1
	for i, line := range lines {
		m := fenceRe.FindStringSubmatch(line)
		if m == nil || m[2] == "" || i <= consumed {
			continue
		}
		end := slices.IndexFunc(lines[i+1:], func(l string) bool {
			return strings.TrimSpace(l) == "```"
		})
		if end < 0 {
			t.Fatalf("%s:%d opens a code block that is never closed", name, i+1)
		}
		body := make([]string, 0, end)
		for _, l := range lines[i+1 : i+1+end] {
			body = append(body, strings.TrimPrefix(l, m[1]))
		}
		out = append(out, fencedBlock{
			line: i + 1,
			info: m[2],
			body: strings.Join(body, "\n") + "\n",
		})
		consumed = i + 1 + end
	}
	return out
}

// topLevelKeys returns the mapping keys a block declares at column zero,
// in document order and without repeats.
func topLevelKeys(body string) []string {
	var out []string
	for _, m := range topLevelKeyRe.FindAllStringSubmatch(body, -1) {
		if !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	return out
}

// subset reports whether got is non-empty and every element of it appears
// in want. Empty is not a subset here on purpose: a console transcript
// declares no keys at column zero, and a block that declares nothing is
// not a configuration this gate should try to load.
func subset(got, want []string) bool {
	for _, g := range got {
		if !slices.Contains(want, g) {
			return false
		}
	}
	return len(got) > 0
}
