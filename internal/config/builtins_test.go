package config

import (
	"sort"
	"strings"
	"testing"

	"github.com/probavi/probavi/internal/i18n"
)

// builtins_test.go pins the check registry from both sides. The registry
// is the vocabulary gate — validation resolves every configured check
// through it — and it is what docs/capabilities.json enumerates, so a kind
// that is registered but unvalidated, or validated but unregistered, would
// mean a published check nothing runs or a runnable check nobody can find.

// minimalCheck returns a check that satisfies a kind's required
// parameters, so the kind can be validated without tripping over unrelated
// rules.
func minimalCheck(k CheckKind) Check {
	c := Check{}
	if k.Builtin {
		c.Builtin = k.ID
	}
	for _, p := range k.Params {
		if !p.Required {
			continue
		}
		switch p.Name {
		case "table":
			c.Table = "orders"
		case "column":
			c.Column = "created_at"
		case "max_age":
			c.MaxAge = Duration(1)
		case "sql":
			c.SQL = "SELECT 1"
		case "expect":
			c.Expect = Scalar{}
		}
	}
	// row_count's extra rule: at least one bound.
	if k.ID == CheckRowCount {
		minBound := int64(0)
		c.Min = &minBound
	}
	if k.ID == CheckSQL {
		// The scalar is set through its YAML unmarshaler, the only path
		// that exists, so the fixture matches what a drill file produces.
		if err := c.Expect.UnmarshalYAML(func(v any) error {
			p, ok := v.(*any)
			if !ok {
				return nil
			}
			*p = any(1)
			return nil
		}); err != nil {
			panic("build expect scalar: " + err.Error())
		}
	}
	return c
}

// TestEveryRegisteredKindIsValidated proves no registry entry is a
// published fiction: each one must be configurable without being rejected
// as unknown.
func TestEveryRegisteredKindIsValidated(t *testing.T) {
	for _, k := range CheckKinds() {
		t.Run(k.ID, func(t *testing.T) {
			c := minimalCheck(k)
			p := &problems{tr: i18n.English()}
			// hasManifest is true because the registry is what is under
			// test here: baseline's one cross-section rule belongs to
			// TestBaselineRequiresAManifest, not to the vocabulary gate.
			c.validate(p, 0, true)
			for _, err := range p.errs {
				if strings.Contains(err.Error(), "unknown builtin") {
					t.Fatalf("registered kind %q is rejected as unknown: %v", k.ID, err)
				}
			}
			if len(p.errs) != 0 {
				t.Errorf("minimal %s check did not validate: %v", k.ID, p.errs)
			}
		})
	}
}

// TestUnregisteredBuiltinIsRejected is the other direction: a builtin the
// registry does not list cannot be configured, which is what stops it from
// ever reaching internal/checks.
func TestUnregisteredBuiltinIsRejected(t *testing.T) {
	for _, name := range []string{"index_valid", "sql", "", "Row_Count"} {
		c := Check{Builtin: name}
		p := &problems{tr: i18n.English()}
		c.validate(p, 0, true)
		if len(p.errs) == 0 {
			t.Errorf("builtin %q was accepted but is not registered", name)
		}
	}
}

// TestRegistryIsWellFormed holds the entries to the fields the
// capabilities manifest publishes.
func TestRegistryIsWellFormed(t *testing.T) {
	kinds := CheckKinds()
	if len(kinds) == 0 {
		t.Fatal("the check registry is empty")
	}
	seen := map[string]bool{}
	builtins := 0
	for _, k := range kinds {
		switch {
		case k.ID == "":
			t.Error("a check kind has no id")
			continue
		case seen[k.ID]:
			t.Errorf("duplicate check kind %q", k.ID)
		case k.Name == "":
			t.Errorf("check %q has no name", k.ID)
		case k.Status == "":
			t.Errorf("check %q has no status", k.ID)
		}
		seen[k.ID] = true
		if k.Builtin {
			builtins++
		}
		for _, p := range k.Params {
			if p.Name == "" || p.Doc == "" || p.Type == "" {
				t.Errorf("check %q has an underspecified parameter %q", k.ID, p.Name)
			}
		}
	}
	if builtins == 0 {
		t.Error("the registry declares no built-in checks")
	}
}

func TestLookupCheckKind(t *testing.T) {
	if k, ok := LookupCheckKind(CheckRowCount); !ok || k.ID != CheckRowCount || !k.Builtin {
		t.Errorf("LookupCheckKind(%q) = %+v, %v", CheckRowCount, k, ok)
	}
	if k, ok := LookupCheckKind(CheckSQL); !ok || k.Builtin {
		t.Errorf("the sql assertion must resolve but not be a builtin: %+v, %v", k, ok)
	}
	if _, ok := LookupCheckKind("no-such-check"); ok {
		t.Error("an unregistered kind resolved")
	}
}

// TestCheckKindsIsACopy proves the registry cannot be mutated through a
// returned slice.
func TestCheckKindsIsACopy(t *testing.T) {
	first := CheckKinds()
	first[0].ID = "tampered"
	if CheckKinds()[0].ID == "tampered" {
		t.Error("CheckKinds returns shared state")
	}
}

// TestNotifyOutcomesMatchesValidation pins the published outcome filter to
// the set a config may actually contain.
func TestNotifyOutcomesMatchesValidation(t *testing.T) {
	got := NotifyOutcomes()
	if len(got) != len(notifyOutcomes) {
		t.Fatalf("NotifyOutcomes() = %v, validation accepts %d values", got, len(notifyOutcomes))
	}
	if !sort.StringsAreSorted(got) {
		t.Errorf("NotifyOutcomes() is not sorted: %v", got)
	}
	for _, o := range got {
		if !notifyOutcomes[o] {
			t.Errorf("NotifyOutcomes() offers %q, which validation rejects", o)
		}
	}
}
