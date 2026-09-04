package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
)

// machineIDPaths are where a Linux installation keeps the identity that
// survives a reboot and does not survive a correct clone. The second is
// the older D-Bus location, still the only one on some distributions.
var machineIDPaths = []string{"/etc/machine-id", "/var/lib/dbus/machine-id"}

// HostID fingerprints this host: the first 16 hex chars of SHA-256 over
// the machine id and the hostname together. Neither leaves the host in
// raw form, and the value fits container-label and Kubernetes character
// rules.
//
// Providers stamp it on every sandbox and scope their orphan sweeps with
// it. That scoping is load-bearing rather than cosmetic: owner-process
// liveness is only checkable on the host that created the sandbox, so a
// sweep that mistakes another host's sandbox for its own asks the wrong
// kernel about the wrong pid and removes a drill that is still running.
// Two drill hosts share a runtime as soon as one points DOCKER_HOST at a
// remote daemon or two of them use one Kubernetes cluster.
//
// Both halves are needed, and measured:
//
//   - The hostname alone collides where it is not unique — cloned virtual
//     machines that kept it, minimal images that default to "localhost".
//   - The machine id alone collides inside containers, where /etc/machine-id
//     is empty in every image measured (alpine, debian) while the hostname
//     is a distinct container id.
//
// Together they disambiguate both, and both are stable across a reboot,
// which the sweep needs: an id that changed on every boot would leave the
// orphans of a crashed run unreclaimable.
//
// What is left is a virtual machine cloned without regenerating its
// machine id, which keeps the hostname too. systemd requires regenerating
// it on clone; an operator who has not is documented in the README as
// needing distinct hostnames before sharing a runtime.
//
// Deliberately not the same value as an evidence record's env.host_id,
// though the two were once the same expression. That field is specified
// as SHA-256 of the hostname (evidence-schema.md §3) and is signed;
// this one is a label nobody reads twice. Unifying them would change what
// a signed field means, which is a schema decision and not a tidy-up.
func HostID() string {
	return hostID(machineID(), hostname())
}

// hostID is the pure half, so the mixing rule can be tested without a
// machine to run it on.
func hostID(machine, host string) string {
	sum := sha256.Sum256([]byte(machine + "\x00" + host))
	return hex.EncodeToString(sum[:])[:16]
}

// machineID returns the first machine identity it can read, or "" when
// there is none — the case on macOS, and inside every container image
// measured. An empty half is not a failure: the hostname carries the id
// on its own there, which is what this function did before it had a
// second half at all.
func machineID() string { return machineIDFrom(machineIDPaths) }

func machineIDFrom(paths []string) string {
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if id := strings.TrimSpace(string(raw)); id != "" {
			return id
		}
	}
	return ""
}

func hostname() string {
	name, err := os.Hostname()
	if err != nil {
		return "unknown-host"
	}
	return name
}
