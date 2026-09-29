package docs_test

import (
	"strings"
	"testing"
)

const (
	mutationWorkflow = ".github/workflows/mutation.yml"
	// touchedJob is the check a branch rule can require, so its name is
	// fixed here as well as there: a rule naming a check that no longer
	// reports does not fail, it waits, and then somebody drops the rule.
	touchedJob     = "touched"
	touchedJobName = "Mutation (touched packages)"
	// touchedJobIf must be true on every pull request. A job that is
	// skipped reports success to a branch rule, so this one may never be
	// conditioned on having work to do — it runs, and says so when there
	// is none.
	touchedJobIf = "github.event_name == 'pull_request'"
	// sweepJob is the weekly run. It must stay off pull requests: the
	// reason the gate is scheduled at all is that fourteen packages is not
	// a question a pull request can afford to ask to learn about the one
	// it changed.
	sweepJob   = "mutate"
	sweepJobIf = "github.event_name != 'pull_request'"
)

// TestTheMutationGateRunsOnPullRequestsAndCannotSkipItself: the per-pull-
// request half of this gate exists because a ceiling that sits at the
// measurement costs latency — a survivor a feature introduces is otherwise
// reported days after the merge, when the author has moved on and the
// tempting fix is the number rather than the assertion. That only works if
// the check is one a branch rule can require and one that cannot report
// success without having run.
func TestTheMutationGateRunsOnPullRequestsAndCannotSkipItself(t *testing.T) {
	raw := read(t, mutationWorkflow)
	if !strings.Contains(raw, "\n  pull_request:\n") {
		t.Fatalf("%s has no pull_request trigger; the gate only speaks a week late", mutationWorkflow)
	}

	jobs := parseWorkflowJobs(t, mutationWorkflow)
	touched, ok := jobs.Jobs[touchedJob]
	if !ok {
		t.Fatalf("%s has no %q job; nothing there can be required on a pull request",
			mutationWorkflow, touchedJob)
	}
	if touched.Name != touchedJobName {
		t.Errorf("%s job %q is named %q, want %q — the branch rule requires that name",
			mutationWorkflow, touchedJob, touched.Name, touchedJobName)
	}
	if touched.If != touchedJobIf {
		t.Errorf("%s job %q has if: %q, want %q — anything narrower lets the job skip, "+
			"and a skipped required check reports success",
			mutationWorkflow, touchedJob, touched.If, touchedJobIf)
	}
	if touched.Needs != nil {
		t.Errorf("%s job %q needs %v — a dependency that fails would skip it, and a skipped "+
			"required check reports success", mutationWorkflow, touchedJob, touched.Needs)
	}

	sweep, ok := jobs.Jobs[sweepJob]
	if !ok {
		t.Fatalf("%s has no %q job; the weekly sweep is the complete answer this one narrows",
			mutationWorkflow, sweepJob)
	}
	if sweep.If != sweepJobIf {
		t.Errorf("%s job %q has if: %q, want %q — the full sweep on every pull request is the "+
			"cost this split exists to avoid", mutationWorkflow, sweepJob, sweep.If, sweepJobIf)
	}
}

// TestTheMutationGateReadsItsScopeFromTheBudget: the declared packages live
// in .mutation-budget, which is also the file that says a package joins the
// run by being declared. A workflow carrying its own copy of that list
// would make the sentence false the first time the two disagreed, and the
// disagreement would be silent — the package nobody added to the workflow
// simply would not be measured on a pull request.
func TestTheMutationGateReadsItsScopeFromTheBudget(t *testing.T) {
	raw := read(t, mutationWorkflow)
	if !strings.Contains(raw, ".mutation-budget") {
		t.Errorf("%s does not read .mutation-budget; its scope is a copy rather than the list",
			mutationWorkflow)
	}
}
