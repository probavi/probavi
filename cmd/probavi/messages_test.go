package main

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"github.com/probavi/probavi/internal/config"
	"github.com/probavi/probavi/internal/i18n"
)

// translatable is the full message set the catalogs are held to: the CLI
// surface plus every package that emits translated diagnostics.
func translatable() []string {
	return append(append([]string{}, allMessages...), config.Messages()...)
}

// verbPattern extracts fmt verbs; translations must carry the same verbs
// in the same order (docs/i18n.md §3).
var verbPattern = regexp.MustCompile(`%[-+ #0-9.*]*[a-zA-Z]`)

// TestCatalogGates enforces docs/i18n.md §4 for every shipped locale:
// complete coverage of the message set, no stale keys, format-verb
// parity. A partially translated language must not ship.
func TestCatalogGates(t *testing.T) {
	tags, err := i18n.Locales()
	if err != nil {
		t.Fatalf("Locales: %v", err)
	}
	if len(tags) == 0 {
		t.Fatal("no embedded catalogs — the hu catalog must exist")
	}
	for _, tag := range tags {
		t.Run(tag, func(t *testing.T) {
			catalog, err := i18n.Catalog(tag)
			if err != nil {
				t.Fatalf("Catalog(%s): %v", tag, err)
			}
			for _, msg := range translatable() {
				assertTranslated(t, catalog, msg)
			}
			assertNoStaleKeys(t, catalog)
		})
	}
}

func assertTranslated(t *testing.T, catalog map[string]string, msg string) {
	t.Helper()
	translated, ok := catalog[msg]
	if !ok {
		t.Errorf("missing translation for %q", msg)
		return
	}
	if translated == "" {
		t.Errorf("empty translation for %q", msg)
		return
	}
	want := verbPattern.FindAllString(msg, -1)
	got := verbPattern.FindAllString(translated, -1)
	if strings.Join(want, ",") != strings.Join(got, ",") {
		t.Errorf("format verbs diverge for %q: source %v, translation %v", msg, want, got)
	}
	if strings.HasSuffix(msg, "\n") != strings.HasSuffix(translated, "\n") {
		t.Errorf("trailing newline diverges for %q", msg)
	}
}

func assertNoStaleKeys(t *testing.T, catalog map[string]string) {
	t.Helper()
	msgs := translatable()
	known := make(map[string]bool, len(msgs))
	for _, m := range msgs {
		known[m] = true
	}
	for key := range catalog {
		if !known[key] {
			t.Errorf("stale catalog key not in the message set: %q", key)
		}
	}
}

// TestHungarianCLIOutput proves the translator reaches the terminal: the
// same invocations that print English by default print Hungarian under
// the hu locale.
func TestHungarianCLIOutput(t *testing.T) {
	hu, err := i18n.New("hu")
	if err != nil {
		t.Fatalf("New(hu): %v", err)
	}
	runHu := func(args ...string) (int, string, string) {
		var out, errBuf bytes.Buffer
		code := run(args, &out, &errBuf, hu)
		return code, out.String(), errBuf.String()
	}

	code, _, stderr := runHu("restore")
	if code != exitUsage {
		t.Fatalf("unknown command exit %d, want %d", code, exitUsage)
	}
	for _, want := range []string{`ismeretlen parancs: "restore"`, "Használat: probavi", "Kilépési kódok"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("hu stderr does not contain %q:\n%s", want, stderr)
		}
	}
	if strings.Contains(stderr, "unknown command") || strings.Contains(stderr, "Usage: probavi") {
		t.Errorf("hu stderr still contains English:\n%s", stderr)
	}

	if _, _, stderr = runHu("run"); !strings.Contains(stderr, "a --config megadása kötelező") {
		t.Errorf("hu run without config: %s", stderr)
	}
	if _, _, stderr = runHu("gameday"); !strings.Contains(stderr, "a --config megadása kötelező") {
		t.Errorf("hu gameday without config: %s", stderr)
	}
	if _, _, stderr = runHu("push"); !strings.Contains(stderr, "a --log megadása kötelező") {
		t.Errorf("hu push without log: %s", stderr)
	}

	code, stdout, _ := runHu("version")
	if code != 0 || !strings.Contains(stdout, "adapterprotokoll:") || !strings.Contains(stdout, "bizonyítékséma:") {
		t.Errorf("hu version output: %s", stdout)
	}
}
