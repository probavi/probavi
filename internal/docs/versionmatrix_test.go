package docs_test

import (
	"slices"
	"testing"

	"github.com/goccy/go-yaml"
)

const (
	// versionMatrixWorkflow re-proves every engine version the manifests
	// claim (docs/engine-versions.md).
	versionMatrixWorkflow = ".github/workflows/version-matrix.yml"
	// gateJob is the job a branch rule requires. Its name is the status
	// check's name, so the two must be spelled the same forever: renaming
	// the job renames the check, and a rule requiring a check that no
	// longer reports does not fail — it waits, and then someone
	// administratively drops it.
	gateJob     = "gate"
	gateJobName = "Version matrix gate"
	// gateJobIf is what separates a gate from a witness. A job whose
	// dependency failed is skipped by default, and a skipped required
	// check reports *success* to a branch rule.
	gateJobIf = "always()"
)

// workflowJobs is the part of a workflow file this file reads.
type workflowJobs struct {
	Jobs map[string]struct {
		Name string `yaml:"name"`
		If   string `yaml:"if"`
		// Needs is a string when a job has one dependency and a list when
		// it has several; GitHub accepts both and this workflow uses both.
		Needs any `yaml:"needs"`
	} `yaml:"jobs"`
}

// TestVersionMatrixGateCoversEveryJob keeps the gate job depending on every
// other job in the workflow.
//
// The matrix is the evidence behind every engine-version claim this
// repository publishes, and until the gate job existed none of it could be
// required on a pull request: `restore` is a fan-out whose check names come
// from the manifest at run time, and a branch rule can only require a
// context it can spell in advance. The gate is the fixed name that stands
// for all of them — which holds exactly as long as it needs all of them. A
// job added here and not added to `needs` would be a hole of precisely the
// shape the gate was built to close.
func TestVersionMatrixGateCoversEveryJob(t *testing.T) {
	jobs := parseWorkflowJobs(t, versionMatrixWorkflow)
	gate, ok := jobs.Jobs[gateJob]
	if !ok {
		t.Fatalf("%s has no %q job; nothing there can be required on a pull request", versionMatrixWorkflow, gateJob)
	}
	if gate.Name != gateJobName {
		t.Errorf("%s job %q is named %q, want %q — the branch rule requires that name",
			versionMatrixWorkflow, gateJob, gate.Name, gateJobName)
	}
	if gate.If != gateJobIf {
		t.Errorf("%s job %q has if: %q, want %q — without it the job is skipped when a dependency fails, "+
			"and a skipped required check reports success", versionMatrixWorkflow, gateJob, gate.If, gateJobIf)
	}
	needs := stringList(t, gate.Needs)
	for id := range jobs.Jobs {
		if id == gateJob || slices.Contains(needs, id) {
			continue
		}
		t.Errorf("%s job %q is not in %q's needs (%v) — a failure there would not fail the gate",
			versionMatrixWorkflow, id, gateJob, needs)
	}
}

// parseWorkflowJobs decodes the jobs of a workflow file.
func parseWorkflowJobs(t *testing.T, name string) workflowJobs {
	t.Helper()
	var wf workflowJobs
	if err := yaml.Unmarshal([]byte(read(t, name)), &wf); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	if len(wf.Jobs) == 0 {
		t.Fatalf("%s declares no jobs", name)
	}
	return wf
}

// stringList normalizes a YAML value that may be a single string or a list
// of them.
func stringList(t *testing.T, v any) []string {
	t.Helper()
	switch typed := v.(type) {
	case string:
		return []string{typed}
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			s, ok := item.(string)
			if !ok {
				t.Fatalf("list holds %T, want string", item)
			}
			out = append(out, s)
		}
		return out
	default:
		t.Fatalf("value is %T, want a string or a list of them", v)
		return nil
	}
}
