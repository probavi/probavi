package cli_test

import (
	"strings"
	"testing"

	"github.com/probavi/probavi/internal/cli"
)

// TestResolve covers every way an argument list can meet the table. The
// distinction between the three failure resolutions is what lets
// cmd/probavi print the diagnostic it printed before the table existed.
func TestResolve(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		want     cli.Resolution
		wantID   string
		wantArgs []string
		wantWord string
		// wantGroup is asserted for every case, empty included: the group
		// word is what cmd/probavi looks up to pick the diagnostic an
		// unknown subcommand gets, so a Match that names the wrong one
		// degrades to "unknown command" in all twenty-three locales — a
		// plausible sentence about the wrong thing.
		wantGroup string
	}{
		{name: "no arguments", args: nil, want: cli.UnknownCommand},
		{name: "top-level command", args: []string{"run", "--config", "d.yaml"},
			want: cli.ResolvedCommand, wantID: cli.CmdRun, wantArgs: []string{"--config", "d.yaml"}},
		{name: "top-level command without arguments", args: []string{"version"},
			want: cli.ResolvedCommand, wantID: cli.CmdVersion, wantArgs: []string{}},
		{name: "subcommand", args: []string{"evidence", "verify", "--log", "l"},
			want: cli.ResolvedCommand, wantID: cli.CmdEvidenceVerify, wantArgs: []string{"--log", "l"}},
		{name: "subcommand of the other group", args: []string{"adapter", "probe", "postgres"},
			want: cli.ResolvedCommand, wantID: cli.CmdAdapterProbe, wantArgs: []string{"postgres"}},
		{name: "unknown command", args: []string{"restore"},
			want: cli.UnknownCommand, wantWord: "restore"},
		{name: "group without subcommand", args: []string{"evidence"},
			want: cli.IncompleteGroup, wantGroup: "evidence"},
		{name: "unknown subcommand", args: []string{"evidence", "sign"},
			want: cli.UnknownSubcommand, wantWord: "sign", wantGroup: "evidence"},
		{name: "unknown subcommand of the other group", args: []string{"adapter", "sign"},
			want: cli.UnknownSubcommand, wantWord: "sign", wantGroup: "adapter"},
		{name: "group word is not a command on its own", args: []string{"adapter"},
			want: cli.IncompleteGroup, wantGroup: "adapter"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := cli.Resolve(tc.args)
			if m.Resolution != tc.want {
				t.Fatalf("resolution %v, want %v", m.Resolution, tc.want)
			}
			if tc.wantID != "" && m.Command.ID != tc.wantID {
				t.Errorf("command %q, want %q", m.Command.ID, tc.wantID)
			}
			if tc.wantWord != "" && m.Word != tc.wantWord {
				t.Errorf("word %q, want %q", m.Word, tc.wantWord)
			}
			if tc.wantArgs != nil {
				assertArgs(t, m.Args, tc.wantArgs)
			}
			// Asserted unconditionally, so that a resolution with no group
			// carries none rather than whatever happened to be in argv.
			if m.Group != tc.wantGroup {
				t.Errorf("group %q, want %q", m.Group, tc.wantGroup)
			}
		})
	}
}

func assertArgs(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("args %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestGroups pins the derived group list: a group exists exactly because
// some command has two words.
func TestGroups(t *testing.T) {
	groups := cli.Groups()
	want := map[string]bool{"evidence": true, "adapter": true}
	if len(groups) != len(want) {
		t.Fatalf("groups %v, want %v", groups, want)
	}
	for _, g := range groups {
		if !want[g] {
			t.Errorf("unexpected group %q", g)
		}
	}
}

// TestTableIsWellFormed holds the contract itself to its invariants: the
// ids downstream consumers read must be unique and match the words a user
// types, and every command must document at least one exit code, because
// the exit-code contract is what cron and CI depend on.
func TestTableIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range cli.Commands() {
		switch {
		case len(c.Words) == 0:
			t.Errorf("command %q has no words", c.ID)
			continue
		case c.ID != strings.Join(c.Words, " "):
			t.Errorf("command id %q does not match its words %v", c.ID, c.Words)
		case seen[c.ID]:
			t.Errorf("duplicate command id %q", c.ID)
		case c.Summary == "":
			t.Errorf("command %q has no summary", c.ID)
		case c.Status == "":
			t.Errorf("command %q has no status", c.ID)
		case len(c.ExitCodes) == 0:
			t.Errorf("command %q documents no exit code", c.ID)
		}
		seen[c.ID] = true
		assertExitCodes(t, c)
		for _, f := range c.Flags {
			if !strings.HasPrefix(f.Name, "--") {
				t.Errorf("command %q: flag %q does not start with --", c.ID, f.Name)
			}
			if f.Doc == "" {
				t.Errorf("command %q: flag %q has no doc", c.ID, f.Name)
			}
		}
	}
}

// assertExitCodes pins the codes to ascending order without duplicates,
// and to the shared vocabulary — an undeclared number would mean the
// binary can return a status the contract does not explain.
func assertExitCodes(t *testing.T, c cli.Command) {
	t.Helper()
	known := map[int]bool{
		cli.ExitPass: true, cli.ExitFail: true, cli.ExitError: true,
		cli.ExitUsage: true, cli.ExitEvidenceLost: true,
	}
	prev := -1
	for _, e := range c.ExitCodes {
		switch {
		case !known[e.Code]:
			t.Errorf("command %q: exit code %d is outside the shared vocabulary", c.ID, e.Code)
		case e.Code <= prev:
			t.Errorf("command %q: exit codes are not ascending at %d", c.ID, e.Code)
		case e.Meaning == "":
			t.Errorf("command %q: exit code %d has no meaning", c.ID, e.Code)
		}
		prev = e.Code
	}
	if len(c.ExitCodes) > 0 && c.ExitCodes[0].Code != cli.ExitPass {
		t.Errorf("command %q: first exit code is %d, want success", c.ID, c.ExitCodes[0].Code)
	}
}

// TestCommandsIsACopy proves the table cannot be mutated through a
// returned slice — it is a contract, not shared state.
func TestCommandsIsACopy(t *testing.T) {
	first := cli.Commands()
	first[0].ID = "tampered"
	if cli.Commands()[0].ID == "tampered" {
		t.Error("Commands returns shared state")
	}
}
