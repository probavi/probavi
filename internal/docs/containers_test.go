package docs_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// dockerRun matches the start of a container invocation in a workflow step
// or a packaging script.
var dockerRun = regexp.MustCompile(`\bdocker run\b`)

// shellReadsStdin matches a shell told to take its program from standard
// input — `sh -s`, `sh -eus`, `bash -s`. That is how every container step
// in this repository passes a script: a quoted heredoc, so the outer shell
// leaves the body alone and the container's shell reads it.
var shellReadsStdin = regexp.MustCompile(`\b(?:ba|a)?sh\s+-[a-z]*s\b`)

// keepsStdinOpen matches docker's interactive flag, in either spelling and
// in a cluster (`-it`, `-ti`). Long options are matched whole so that
// `--init` and `--privileged` — both of which contain an i — cannot pass
// for it.
var keepsStdinOpen = regexp.MustCompile(`\s(?:--interactive|-[a-z]*i[a-z]*)(?:\s|$)`)

// continued reports whether a shell line continues onto the next one.
func continued(line string) bool {
	return strings.HasSuffix(strings.TrimRight(line, " \t"), `\`)
}

// logicalLines joins backslash-continued lines, so a `docker run` split
// across five lines for readability is examined as the one command it is.
// The line number returned is that of the first physical line, which is
// what a reader needs to find it.
func logicalLines(content string) []struct {
	num  int
	text string
} {
	var out []struct {
		num  int
		text string
	}
	lines := strings.Split(content, "\n")
	for i := 0; i < len(lines); i++ {
		start, joined := i+1, strings.TrimRight(lines[i], " \t")
		for continued(joined) && i+1 < len(lines) {
			i++
			joined = strings.TrimSuffix(joined, `\`) + " " + strings.TrimSpace(lines[i])
			joined = strings.TrimRight(joined, " \t")
		}
		out = append(out, struct {
			num  int
			text string
		}{start, joined})
	}
	return out
}

// containerCallers are the files that run a container and feed it a
// script: the workflows, and the packaging scripts both workflows share.
func containerCallers(t *testing.T) []string {
	t.Helper()
	var files []string
	for _, pattern := range []string{".github/workflows/*.yml", "packaging/*.sh"} {
		matches, err := filepath.Glob(filepath.Join(repoRoot, pattern))
		if err != nil {
			t.Fatalf("glob %s: %v", pattern, err)
		}
		if len(matches) == 0 {
			t.Fatalf("%s matched nothing — this gate would pass vacuously", pattern)
		}
		for _, m := range matches {
			rel, err := filepath.Rel(repoRoot, m)
			if err != nil {
				t.Fatalf("relativise %s: %v", m, err)
			}
			files = append(files, rel)
		}
	}
	return files
}

// TestContainerHeredocsReachTheShell keeps a container step from passing
// by running nothing at all.
//
// `docker run` without -i gives the container an empty standard input. A
// shell told to read its program from there — `sh -eus` — then reads end
// of file, executes nothing, and exits 0. The step is green, the log
// shows the image being pulled and not one line of output after it, and
// the assertions inside the heredoc never happen.
//
// This is not hypothetical. ci.yml's three package jobs installed the
// .deb, .rpm and .apk into Debian, Fedora and Alpine and asked the core
// to resolve an adapter — the check that catches a package putting a
// binary where the PATH lookup will never find it. From the commit that
// introduced them on 2026-08-05 until the one that added this test, all
// three ran nothing, on every pull request and every release. A gate that
// weakens itself is worse than no gate: it also answers the question of
// whether anyone is checking.
func TestContainerHeredocsReachTheShell(t *testing.T) {
	for _, file := range containerCallers(t) {
		raw, err := os.ReadFile(filepath.Join(repoRoot, file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for _, line := range logicalLines(string(raw)) {
			if !dockerRun.MatchString(line.text) || !strings.Contains(line.text, "<<") {
				continue
			}
			if !shellReadsStdin.MatchString(line.text) {
				continue
			}
			if keepsStdinOpen.MatchString(line.text) {
				continue
			}
			t.Errorf("%s:%d feeds a heredoc to a shell reading stdin without -i, so the "+
				"container runs nothing and the step passes regardless: %s",
				file, line.num, strings.TrimSpace(line.text))
		}
	}
}

// TestTheHeredocGateWouldCatchItReversed proves the gate above can fail,
// because a regular expression that matches nothing is indistinguishable
// from one that finds nothing wrong.
func TestTheHeredocGateWouldCatchItReversed(t *testing.T) {
	cases := []struct {
		name  string
		line  string
		wants bool
	}{
		{"no -i, one line", `docker run --rm -v "$PWD:/x" alpine:3.22 sh -eus <<'SH'`, true},
		{"no -i, continued", "docker run --rm \\\n  -v \"$PWD:/x\" \\\n  alpine:3.22 sh -eus <<'SH'", true},
		{"with -i", `docker run --rm -i -v "$PWD:/x" alpine:3.22 sh -eus <<'SH'`, false},
		{"with -it", `docker run --rm -it alpine:3.22 sh -eus <<'SH'`, false},
		{"long spelling", `docker run --rm --interactive alpine:3.22 sh -eus <<'SH'`, false},
		{"--init is not -i", `docker run --rm --init alpine:3.22 sh -eus <<'SH'`, true},
		{"script as an argument needs no stdin", `docker run --rm alpine:3.22 sh -c 'echo hi' # <<'SH'`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var flagged bool
			for _, line := range logicalLines(tc.line) {
				if !dockerRun.MatchString(line.text) || !strings.Contains(line.text, "<<") {
					continue
				}
				if !shellReadsStdin.MatchString(line.text) {
					continue
				}
				if !keepsStdinOpen.MatchString(line.text) {
					flagged = true
				}
			}
			if flagged != tc.wants {
				t.Errorf("flagged = %v, want %v for %q", flagged, tc.wants, tc.line)
			}
		})
	}
}
