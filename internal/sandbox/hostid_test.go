package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestHostIDShape pins what every provider stamps on a sandbox: a value
// that fits a container label and a Kubernetes label alike, and does not
// move between two calls in one process.
func TestHostIDShape(t *testing.T) {
	id := HostID()
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(id) {
		t.Fatalf("HostID() = %q, want 16 lowercase hex chars", id)
	}
	if again := HostID(); again != id {
		t.Errorf("HostID is not deterministic: %q then %q", id, again)
	}
}

// TestHostIDIsNotTheEvidenceHostID guards the tidy-up nobody should make.
//
// An evidence record's env.host_id is specified as SHA-256 of the
// hostname (evidence-schema.md §3) and is signed. This id is a sweep
// label. They were once the same expression, and unifying them again
// would change what a signed field means — a schema decision, not a
// refactor. The two differing is the point.
func TestHostIDIsNotTheEvidenceHostID(t *testing.T) {
	name, err := os.Hostname()
	if err != nil {
		t.Skipf("hostname unavailable: %v", err)
	}
	sum := sha256.Sum256([]byte(name))
	evidenceShape := hex.EncodeToString(sum[:])[:16]
	if HostID() == evidenceShape && machineID() != "" {
		t.Error("HostID() equals the evidence env.host_id derivation — the sweep id must not be the signed one")
	}
}

// TestHostIDSeparatesHostsTheOtherHalfCannot is the whole reason there are
// two halves.
//
// A sweep decides "mine" by comparing this value, then asks the local
// kernel whether the owner pid is alive. Two hosts sharing a value ask
// the wrong kernel about the wrong pid and remove a drill that is still
// running, so the cases below are the ones that must not collide —
// measured: a container's /etc/machine-id is empty in every image tried,
// while a cloned virtual machine keeps its hostname.
func TestHostIDSeparatesHostsTheOtherHalfCannot(t *testing.T) {
	tests := map[string]struct{ aMachine, aHost, bMachine, bHost string }{
		"cloned VM, machine id regenerated, hostname kept": {
			"11111111111111111111111111111111", "drills",
			"22222222222222222222222222222222", "drills",
		},
		"two containers, no machine id, distinct hostnames": {
			"", "9ee995b54aaa",
			"", "8186d71727a5",
		},
		"both halves differ": {
			"11111111111111111111111111111111", "alpha",
			"22222222222222222222222222222222", "beta",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if a, b := hostID(tt.aMachine, tt.aHost), hostID(tt.bMachine, tt.bHost); a == b {
				t.Errorf("both hosts got %q — a sweep on one would reclaim the other's live sandboxes", a)
			}
		})
	}
}

// TestHostIDIsStableAcrossReboots states the requirement that rules out
// the obvious alternative. A boot id would separate every host, and would
// also change on every restart — leaving the orphans of a crashed run
// unreclaimable, which is the case the sweep exists for.
func TestHostIDIsStableAcrossReboots(t *testing.T) {
	const machine, host = "11111111111111111111111111111111", "drills"
	if first, second := hostID(machine, host), hostID(machine, host); first != second {
		t.Errorf("same machine and hostname gave %q then %q", first, second)
	}
}

// TestHostIDHalvesCannotBeSwapped covers the separator. Concatenating the
// two halves without one would let a different split of the same bytes
// produce the same id.
func TestHostIDHalvesCannotBeSwapped(t *testing.T) {
	if a, b := hostID("ab", "c"), hostID("a", "bc"); a == b {
		t.Errorf("hostID(%q, %q) and hostID(%q, %q) both gave %q", "ab", "c", "a", "bc", a)
	}
}

func TestMachineIDFrom(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return path
	}
	filled := write("filled", "79a69fb6d9584a448815adb3b5dedded\n")
	// What a container image ships: the file exists and says nothing.
	empty := write("empty", "\n")
	missing := filepath.Join(dir, "missing")

	for name, tt := range map[string]struct {
		paths []string
		want  string
	}{
		"reads the first that has something": {[]string{filled, empty}, "79a69fb6d9584a448815adb3b5dedded"},
		"skips an empty file":                {[]string{empty, filled}, "79a69fb6d9584a448815adb3b5dedded"},
		"skips a missing file":               {[]string{missing, filled}, "79a69fb6d9584a448815adb3b5dedded"},
		"no identity at all":                 {[]string{missing, empty}, ""},
		"nowhere to look":                    {nil, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := machineIDFrom(tt.paths); got != tt.want {
				t.Errorf("machineIDFrom(%v) = %q, want %q", tt.paths, got, tt.want)
			}
		})
	}
}
