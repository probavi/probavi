package registry_test

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/probavi/probavi/internal/sandbox/registry"
)

// endpointWords name a machine to reach rather than a property of the
// sandbox to create. docs/sandbox-providers.md §5 forbids a parameter
// carrying one, because sandbox.params is copied into the signed record
// verbatim and unfiltered and the evidence schema forbids connection
// details anywhere in a record (§8). All three shipped providers take
// their endpoint from the environment instead — DOCKER_HOST, KUBECONFIG,
// PROBAVI_SSH_TARGET — and each says so in its own constraints.
var endpointWords = []string{
	"addr", "address", "daemon", "endpoint", "host", "hostname",
	"port", "server", "socket", "target", "uri", "url",
}

// credentialWords name something that authenticates. The rule is the
// same one and sharper: no provider accepts a secret through config,
// because a record would carry it and an evidence log is append-only.
var credentialWords = []string{
	"auth", "cert", "credential", "credentials", "identity", "key",
	"passphrase", "password", "secret", "token",
}

// segments splits a config key the way an operator reads it. Whole
// segments are compared rather than substrings, so "keyspace" and
// "network" are not endpoints while "ssh_key" and "repo.host" are.
var segments = regexp.MustCompile(`[^a-z0-9]+`)

// forbiddenWord reports the reserved word a parameter name carries, and
// which rule it belongs to.
func forbiddenWord(name string) (word, rule string) {
	for _, s := range segments.Split(strings.ToLower(name), -1) {
		if slices.Contains(endpointWords, s) {
			return s, "an endpoint is not a parameter"
		}
		if slices.Contains(credentialWords, s) {
			return s, "a credential is not a parameter"
		}
	}
	return "", ""
}

// TestNoDescriptorDeclaresAnEndpointOrACredential makes
// docs/sandbox-providers.md §5 true rather than merely written. Until
// this test existed, a provider that declared `host` or `endpoint` would
// compile, validate, and sign it: nothing inspects a parameter value, and
// config validation checks little more than that a provider was named.
//
// The list above is the place that conversation has to happen. A
// candidate provider that genuinely needs an endpoint in configuration
// cannot quietly acquire one — it has to remove a word from this test and
// say why, which is exactly what the ROADMAP's first sandbox door asks to
// happen before such a provider exists rather than during it.
func TestNoDescriptorDeclaresAnEndpointOrACredential(t *testing.T) {
	descriptors := registry.Descriptors()
	if len(descriptors) == 0 {
		t.Fatal("the registry ships no providers, so this gate would pass vacuously")
	}
	for _, d := range descriptors {
		for _, p := range d.Params {
			if word, rule := forbiddenWord(p.Name); word != "" {
				t.Errorf("provider %q declares parameter %q, whose %q makes it a connection detail or "+
					"a secret — %s (docs/sandbox-providers.md §5). sandbox.params is copied into the "+
					"signed record verbatim; put it in the environment the way DOCKER_HOST, KUBECONFIG "+
					"and PROBAVI_SSH_TARGET are",
					d.ID, p.Name, word, rule)
			}
		}
	}
}

// TestForbiddenWordMatchesWholeSegments is what keeps the gate above from
// passing because it recognises nothing. Substring matching would refuse
// every parameter with "key" inside a longer word; segment matching
// refuses the ones that are actually a machine or a credential.
func TestForbiddenWordMatchesWholeSegments(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		// Every parameter the three shipped providers declare.
		{"image", ""},
		{"network", ""},
		{"memory", ""},
		{"cpus", ""},
		{"command", ""},
		{"env.", ""},
		{"namespace", ""},
		{"workspace_root", ""},
		// Shapes a remote provider would reach for first.
		{"host", "host"},
		{"endpoint", "endpoint"},
		{"daemon_socket", "daemon"},
		{"remote.host", "host"},
		{"api_url", "url"},
		{"ssh_target", "target"},
		{"ssh_key", "key"},
		{"api_token", "token"},
		{"Password", "password"},
		// Words that merely contain a reserved one are not it.
		{"keyspace", ""},
		{"porthole", ""},
		{"hostile", ""},
		{"certainty", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, rule := forbiddenWord(tt.name)
			if got != tt.want {
				t.Fatalf("forbiddenWord(%q) = %q, want %q", tt.name, got, tt.want)
			}
			if (rule == "") != (tt.want == "") {
				t.Errorf("forbiddenWord(%q) returned word %q with rule %q — a refusal must say which rule",
					tt.name, got, rule)
			}
		})
	}
}
