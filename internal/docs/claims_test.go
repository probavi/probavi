package docs_test

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// manifest is the subset of docs/capabilities.json this gate reads. The
// manifest is generated from the code that implements each capability, so
// it is the only thing in the repository that cannot be out of date.
type manifest struct {
	Adapters []struct {
		ID    string  `json:"id"`
		Name  string  `json:"name"`
		Since *string `json:"since"`
	} `json:"adapters"`
	SandboxProviders []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"sandbox_providers"`
	Locales struct {
		Available []string `json:"available"`
	} `json:"locales"`
}

func readManifest(t *testing.T) manifest {
	t.Helper()
	var m manifest
	if err := json.Unmarshal([]byte(read(t, "docs/capabilities.json")), &m); err != nil {
		t.Fatalf("parse capabilities manifest: %v", err)
	}
	if len(m.Adapters) == 0 || len(m.SandboxProviders) == 0 || len(m.Locales.Available) == 0 {
		t.Fatal("capabilities manifest is empty — this gate would pass vacuously")
	}
	return m
}

// TestReadmeNamesEveryShippedCapability keeps the README from understating
// what ships. AGENTS.md §5.8 makes docs/capabilities.json the only
// permitted source of capability claims for downstream surfaces, and CI
// regenerates it from the code — but nothing tied the README to it, and the
// audit found three claims that had quietly gone stale: a provider listed
// as future work months after it shipped, a sandbox list missing it, and a
// localization paragraph describing 1 of the 24 languages that were
// already there.
func TestReadmeNamesEveryShippedCapability(t *testing.T) {
	m := readManifest(t)
	readme := read(t, sourceDoc)

	for _, a := range m.Adapters {
		if !strings.Contains(readme, a.ID) && !strings.Contains(readme, a.Name) {
			t.Errorf("%s never mentions the %s adapter, which ships", sourceDoc, a.ID)
		}
	}
	for _, p := range m.SandboxProviders {
		if !strings.Contains(readme, p.ID) && !strings.Contains(readme, p.Name) {
			t.Errorf("%s never mentions the %s sandbox provider, which ships", sourceDoc, p.ID)
		}
	}
}

// futureMarker matches prose that puts something in the future.
var futureMarker = regexp.MustCompile(`\b(later|planned|coming soon|not yet|arrives?|will arrive|will be added|in a future release)\b`)

// TestReadmeDoesNotDeferShippedCapabilities is the other half: a
// capability can be present in the README and still be described as
// something that has not happened. "Docker containers and Kubernetes Jobs
// today, remote hosts later" named the provider and was wrong anyway.
//
// The rule is narrow on purpose — only lines that name a shipped adapter
// or provider are read, so ordinary forward-looking prose about the
// roadmap is untouched.
func TestReadmeDoesNotDeferShippedCapabilities(t *testing.T) {
	m := readManifest(t)
	names := make([]string, 0, len(m.Adapters)+len(m.SandboxProviders))
	for _, a := range m.Adapters {
		names = append(names, a.ID, a.Name)
	}
	for _, p := range m.SandboxProviders {
		names = append(names, p.ID, p.Name)
	}

	for i, line := range strings.Split(read(t, sourceDoc), "\n") {
		lower := strings.ToLower(line)
		marker := futureMarker.FindString(lower)
		if marker == "" {
			continue
		}
		for _, name := range names {
			if strings.Contains(lower, strings.ToLower(name)) {
				t.Errorf("%s:%d: %q describes %s as future work, but it ships:\n  %s",
					sourceDoc, i+1, marker, name, strings.TrimSpace(line))
				break
			}
		}
	}
}
