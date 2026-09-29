package k8s

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/probavi/probavi/internal/sandbox"
)

// mutation_test.go holds the cases a mutation run found missing: each one
// is a change to the code that every other test accepted. They are here
// together because that is what they have in common — a gap in what the
// suite asserts rather than a gap in what it covers.

// TestAnExecIsBoundedWhenItAskedToBe: zero means the caller asked for no
// bound, so attaching one hands every exec a context that has already
// expired; and a bound the guard steps past is a command the drill's own
// clock cannot reach.
func TestAnExecIsBoundedWhenItAskedToBe(t *testing.T) {
	for name, tc := range map[string]struct {
		timeout time.Duration
		want    bool
	}{
		"no timeout asked for": {0, false},
		"the smallest timeout": {1, true},
		"an ordinary timeout":  {time.Second, true},
	} {
		t.Run(name, func(t *testing.T) {
			sbx, fake := testSandbox(t, response{})
			if _, err := sbx.Exec(context.Background(), sandbox.ExecRequest{
				Argv: []string{"true"}, Timeout: tc.timeout,
			}); err != nil {
				t.Fatalf("Exec: %v", err)
			}
			if len(fake.bounded) != 1 {
				t.Fatalf("calls = %d, want one exec", len(fake.bounded))
			}
			if fake.bounded[0] != tc.want {
				t.Errorf("context carried a deadline = %v, want %v", fake.bounded[0], tc.want)
			}
		})
	}
}

// TestTheSmallestInputStillOpensStdin: kubectl exec attaches stdin only
// with -i, and this provider needs it for two separate reasons — bytes to
// deliver, and an environment applied through env(1), which is read from
// the same stream. A guard that steps past one of either drops the smallest
// case there is while the caller believes it was served.
func TestTheSmallestInputStillOpensStdin(t *testing.T) {
	for name, req := range map[string]sandbox.ExecRequest{
		"one byte of stdin and no environment": {Argv: []string{"cat"}, Stdin: []byte("x")},
		"one variable and no stdin":            {Argv: []string{"true"}, Env: map[string]string{"A": "1"}},
	} {
		t.Run(name, func(t *testing.T) {
			sbx, fake := testSandbox(t, response{})
			if _, err := sbx.Exec(context.Background(), req); err != nil {
				t.Fatalf("Exec: %v", err)
			}
			if !slices.Contains(fake.calls[0], "-i") {
				t.Errorf("argv = %v, want -i", fake.calls[0])
			}
		})
	}
}

// TestAnEmptyNamespaceIsNoNamespace: the default is what a drill that named
// none gets, and a named-but-empty one is the same statement. Taking it
// literally sends the Job to `-n ""`, which is not the default namespace —
// it is no namespace at all, and the pod holding restored production data
// lands wherever the cluster decides to put it.
func TestAnEmptyNamespaceIsNoNamespace(t *testing.T) {
	p, _ := testProvider(t)
	for name, params := range map[string]map[string]string{
		"no namespace named": {"image": "postgres:16"},
		"named but empty":    {"image": "postgres:16", "namespace": ""},
		"named with spaces":  {"image": "postgres:16", "namespace": "drills"},
	} {
		t.Run(name, func(t *testing.T) {
			_, namespace, err := p.manifest(Descriptor, params)
			if err != nil {
				t.Fatalf("manifest: %v", err)
			}
			want := params["namespace"]
			if want == "" {
				want = "default"
			}
			if namespace != want {
				t.Errorf("namespace = %q, want %q", namespace, want)
			}
		})
	}
}

// TestOneLimitIsStillGuaranteed: requests are set equal to limits so that a
// pod holding restored production data is not evicted under node pressure.
// A guard that waits for a second limit leaves a drill configured with only
// memory — or only cpus — in the burstable class, where the eviction it was
// written to prevent is exactly what happens.
func TestOneLimitIsStillGuaranteed(t *testing.T) {
	p, _ := testProvider(t)
	for name, params := range map[string]map[string]string{
		"memory alone": {"image": "postgres:16", "memory": "2Gi"},
		"cpus alone":   {"image": "postgres:16", "cpus": "2"},
	} {
		t.Run(name, func(t *testing.T) {
			m, _, err := p.manifest(Descriptor, params)
			if err != nil {
				t.Fatalf("manifest: %v", err)
			}
			res := m.Spec.Template.Spec.Containers[0].Resources
			if res == nil {
				t.Fatal("no resources block; the pod is burstable and can be evicted mid-restore")
			}
			if len(res.Limits) != 1 || len(res.Requests) != len(res.Limits) {
				t.Errorf("limits %v, requests %v — requests must equal limits", res.Limits, res.Requests)
			}
		})
	}
}

// TestASuffixThatDoesNotRepeat: the suffix keeps two Jobs created in one
// second from colliding on a name. The fallback for a failing crypto/rand
// is the pid, which is the same for every Job this process makes — correct
// as a fallback, a collision generator as the normal path.
func TestASuffixThatDoesNotRepeat(t *testing.T) {
	seen := map[string]bool{}
	for range 8 {
		s := randomSuffix()
		if s == "" || seen[s] {
			t.Fatalf("suffix %q is empty or repeated; Job names would collide", s)
		}
		seen[s] = true
	}
}

// TestFactsAreAbsentWhenKubectlDidNotAnswer: a non-zero exit is not an
// error by the Runner contract — err stays nil — so a check that reads only
// err believes whatever kubectl printed on its way to failing. The existing
// case cannot see this, because its failing call prints nothing: it is a
// failure that still parses to the same emptiness. This one prints a
// perfectly good pod.
func TestFactsAreAbsentWhenKubectlDidNotAnswer(t *testing.T) {
	good := "docker.io/library/postgres@sha256:" + strings.Repeat("7a", 32) + " 2Gi 1500m"
	for name, r := range map[string]response{
		"a non-zero exit that still printed a pod": {stdout: good, stderr: "error: not found", exit: 1},
		"a spawn failure that still printed":       {stdout: good, err: errors.New("kubectl missing")},
	} {
		t.Run(name, func(t *testing.T) {
			sbx, _ := testSandbox(t, r)
			if got := sbx.Facts(context.Background()); got != (sandbox.Facts{}) {
				t.Errorf("facts = %+v, want none: kubectl did not answer", got)
			}
		})
	}
}

// TestFactsSurviveAPodWithFewerFieldsThanExpected: the jsonpath asks for
// three values and a pod that sets no cpu limit answers with fewer. Each
// index is guarded for that reason, and the guards are the difference
// between a missing field and a panic inside a drill that had already
// restored.
func TestFactsSurviveAPodWithFewerFieldsThanExpected(t *testing.T) {
	digest := "sha256:" + strings.Repeat("7a", 32)
	sbx, _ := testSandbox(t, response{stdout: "docker.io/library/postgres@" + digest + " 2Gi"})
	facts := sbx.Facts(context.Background())
	if facts.ImageDigest == nil || *facts.ImageDigest != digest {
		t.Errorf("image digest = %v, want %q", facts.ImageDigest, digest)
	}
	if facts.MemoryBytes == nil || *facts.MemoryBytes != 2147483648 {
		t.Errorf("memory = %v, want 2Gi", facts.MemoryBytes)
	}
	if facts.CPUsMilli != nil {
		t.Errorf("cpu = %d, want none: the pod did not state one", *facts.CPUsMilli)
	}
}

// TestAQuantityIsKeptWhenItRoundsToSomething: the conversion to thousandths
// is where a quantity becomes a record field, and both edges were
// unasserted. A limit that rounds to nothing must stay absent rather than
// be recorded as a limit of zero, which is what §6.1 means by null; and the
// smallest quantity that does round to something is still a limit somebody
// set.
func TestAQuantityIsKeptWhenItRoundsToSomething(t *testing.T) {
	for in, want := range map[string]int64{
		"0.001":  1, // the smallest that survives the conversion
		"0.0004": 0, // rounds away: absent, never a limit of zero
		"0.0":    0,
	} {
		got := quantityMilli(in)
		switch {
		case want == 0 && got != nil:
			t.Errorf("quantityMilli(%q) = %d, want nothing recorded", in, *got)
		case want != 0 && (got == nil || *got != want):
			t.Errorf("quantityMilli(%q) = %v, want %d", in, got, want)
		}
	}
	// A quantity too large for a float is not a limit either. It reaches
	// the conversion as an infinity, and what an infinity becomes as an
	// int64 is not something a signed record may depend on.
	if got := quantityBytes("1e400"); got != nil {
		t.Errorf("quantityBytes(1e400) = %d, want nothing recorded", *got)
	}
}

// TestACreateThatCouldNotBeUndoneSaysSo: the Job exists and nothing outside
// the call knows its name, so a failure to destroy leaks a pod that no
// later sweep can attribute. The log line is the whole of what an operator
// gets, and the condition deciding whether it is written accepted being
// reversed.
func TestACreateThatCouldNotBeUndoneSaysSo(t *testing.T) {
	p, _ := testProvider(t,
		response{}, // kubectl apply
		response{err: errors.New("api server gone")}, // await: get pods fails
		response{err: errors.New("api server gone")}, // delete: destroy fails
	)
	log := &bytes.Buffer{}
	p.logger = slog.New(slog.NewTextHandler(log, nil))

	if _, err := p.Create(context.Background(), map[string]string{"image": "postgres:16"}); err == nil {
		t.Fatal("Create reported success although the pod never ran")
	}
	if !strings.Contains(log.String(), "destroy after failed create") {
		t.Errorf("logged %q, want the leaked Job named", log)
	}
}
