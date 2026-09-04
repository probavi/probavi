package sandbox

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// envNamePattern is what a name has to look like before it may be
// exported into a sandbox. It is the POSIX shell's own rule for a
// portable variable name, and it lives here — beside the prelude that
// consumes such names — so the providers that accept `env.` parameters
// and the verb that carries them at run time cannot drift apart.
var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidEnvName reports whether name may be exported into a sandbox.
func ValidEnvName(name string) bool { return envNamePattern.MatchString(name) }

// EnvPreludeScript returns a POSIX shell program that reads exactly n
// "NAME=value" lines from stdin, exports each, and then execs the command
// it is given as arguments. Whatever follows those n lines on stdin is the
// command's own stdin, untouched.
//
// It exists because two providers have no out-of-band way to set the
// environment of a command they run: kubectl exec has no environment flag,
// and ssh's SendEnv depends on the target server's AcceptEnv. Putting the
// values on the command line instead — `env NAME=value cmd` — publishes
// every secret a check needs to the process list, on the drill host and
// again on the target. internal/checks refuses {{password}} in an
// sql_runner argv for exactly that reason; a provider must not undo it.
//
// A shell reading a pipe consumes one byte at a time for `read`, so the
// prelude cannot swallow bytes meant for the command. `sh` is already a
// requirement of these providers (put_file needs it too).
func EnvPreludeScript(n int) string {
	return fmt.Sprintf(
		`n=%d; while [ "$n" -gt 0 ]; do IFS= read -r probavi_env || exit 125; `+
			`export "$probavi_env"; n=$((n-1)); done; unset probavi_env; exec "$@"`, n)
}

// EnvPreludeLines renders env as the newline-terminated block
// EnvPreludeScript expects, sorted by name so a command line is
// reproducible.
//
// A value containing a newline cannot be expressed in a line protocol.
// Rather than silently truncating a credential — or worse, exporting its
// tail as another variable — such a value is rejected: a caller that needs
// one has hit a real limitation and deserves to be told.
//
// The name is held to the same line and rather more: it is checked before
// it is written, because a name is not only data here. A newline in one
// makes the rendered block longer than the count the prelude was told to
// read, so the lines past that count stop being environment and become
// the command's stdin — carrying, in the worst arrangement, the next
// variable's value. Anything that is not a portable shell name is refused
// rather than shaped into one: names reach this through the exec verb,
// from an adapter, and an adapter that asks for something impossible
// should be told so rather than have its request quietly altered.
func EnvPreludeLines(env map[string]string) (string, error) {
	names := make([]string, 0, len(env))
	for k := range env {
		names = append(names, k)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, k := range names {
		if !ValidEnvName(k) {
			return "", fmt.Errorf("%w: %q is not a usable environment variable name for the sandbox", ErrInvalidParams, k)
		}
		if strings.ContainsAny(env[k], "\n\r") {
			return "", fmt.Errorf("%w: environment value for %s contains a newline and cannot be passed to the sandbox", ErrInvalidParams, k)
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(env[k])
		b.WriteByte('\n')
	}
	return b.String(), nil
}
