package sandbox

import (
	"errors"
	"strings"
	"testing"
)

func TestEnvPreludeLines(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"empty", nil, ""},
		{"single", map[string]string{"A": "1"}, "A=1\n"},
		{"sorted by name", map[string]string{"B": "2", "A": "1", "C": "3"}, "A=1\nB=2\nC=3\n"},
		{"values may contain anything but newlines", map[string]string{
			"P": `s3cr3t "quoted" $(not-expanded) 'single' \back\`,
		}, "P=" + `s3cr3t "quoted" $(not-expanded) 'single' \back\` + "\n"},
		{"empty value", map[string]string{"A": ""}, "A=\n"},
		{"value with spaces and tabs", map[string]string{"A": "one two\tthree"}, "A=one two\tthree\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := EnvPreludeLines(tt.env)
			if err != nil {
				t.Fatalf("EnvPreludeLines: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
			if n := strings.Count(got, "\n"); n != len(tt.env) {
				t.Errorf("%d lines for %d variables — the prelude reads a fixed count", n, len(tt.env))
			}
		})
	}
}

// TestEnvPreludeLinesRejectsNewlines pins the one value the line protocol
// cannot express. Silently truncating a credential, or exporting its tail
// as a second variable, would both be worse than refusing it.
func TestEnvPreludeLinesRejectsNewlines(t *testing.T) {
	for _, value := range []string{"two\nlines", "trailing\n", "\rcarriage", "a\r\nb"} {
		_, err := EnvPreludeLines(map[string]string{"PGPASSWORD": value})
		if err == nil || !errors.Is(err, ErrInvalidParams) {
			t.Errorf("EnvPreludeLines(%q) err = %v, want an invalid-params rejection", value, err)
		}
		if err != nil && strings.Contains(err.Error(), value) {
			t.Errorf("error text echoes the value, which may be a credential: %v", err)
		}
	}
}

func TestEnvPreludeScript(t *testing.T) {
	// The count is what makes the prelude stop reading at the boundary
	// between the env block and the command's own stdin.
	for _, n := range []int{0, 1, 7} {
		script := EnvPreludeScript(n)
		if !strings.Contains(script, "n="+itoa(n)+";") {
			t.Errorf("script for %d does not carry the count: %s", n, script)
		}
		if !strings.HasSuffix(script, `exec "$@"`) {
			t.Errorf("script must exec the command so its exit code survives: %s", script)
		}
		if strings.ContainsAny(script, "\n\r") {
			t.Errorf("script must stay a single argument: %q", script)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

// TestEnvPreludeLinesRefusesAnUnusableName covers the half of the line
// protocol the value check did not.
//
// A name is not only data here: the prelude is told how many lines to
// read, so a newline inside one makes the block longer than that count
// and everything past it stops being environment and becomes the
// command's stdin — the next variable's value included. The rest are
// refused on the same rule rather than on that consequence, because a
// name the shell cannot export is a request the caller should hear about.
func TestEnvPreludeLinesRefusesAnUnusableName(t *testing.T) {
	for name, key := range map[string]string{
		"a newline in the name": "PGPASSWORD\nEVIL",
		"a carriage return":     "PG\rPASSWORD",
		"an equals sign":        "PG=PASSWORD",
		"a leading digit":       "1PGPASSWORD",
		"a dash":                "PG-PASSWORD",
		"a space":               "PG PASSWORD",
		"empty":                 "",
		"a shell metacharacter": "PG$(id)",
	} {
		t.Run(name, func(t *testing.T) {
			out, err := EnvPreludeLines(map[string]string{key: "secret"})
			if err == nil {
				t.Fatalf("EnvPreludeLines accepted %q, rendering %q", key, out)
			}
			if !errors.Is(err, ErrInvalidParams) {
				t.Errorf("error = %v, want it to wrap ErrInvalidParams", err)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Errorf("error %q carries the value — a refusal must not leak what it refused", err)
			}
		})
	}
}

// TestEnvPreludeLinesAcceptsPortableNames is the other side: the rule must
// not refuse what a drill legitimately sets.
func TestEnvPreludeLinesAcceptsPortableNames(t *testing.T) {
	env := map[string]string{"PGPASSWORD": "s3cret", "_HIDDEN": "x", "MYSQL_PWD9": "y"}
	out, err := EnvPreludeLines(env)
	if err != nil {
		t.Fatalf("EnvPreludeLines: %v", err)
	}
	if got := strings.Count(out, "\n"); got != len(env) {
		t.Errorf("rendered %d lines, want %d — the prelude reads exactly that count", got, len(env))
	}
}
