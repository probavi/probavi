package capabilities

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/probavi/probavi/internal/adapter"
	"github.com/probavi/probavi/internal/cli"
	"github.com/probavi/probavi/internal/config"
	"github.com/probavi/probavi/internal/evidence"
	"github.com/probavi/probavi/internal/i18n"
	"github.com/probavi/probavi/internal/notify"
	"github.com/probavi/probavi/internal/push"
	"github.com/probavi/probavi/internal/sandbox"
	"github.com/probavi/probavi/internal/sandbox/registry"
)

// Build assembles the capabilities document from the repository rooted at
// root. Ordering is deterministic throughout: adapters are discovered on
// the filesystem and therefore sorted by id, while the declared registries
// keep their documentation order, which is stable and carries meaning a
// lexicographic sort would destroy.
//
// The registries are read here and handed to the section builders, rather
// than reached for inside them. That keeps every validation reachable from
// a test: a compiled-in registry cannot be given a bad entry, but a
// section builder can.
func Build(root string) (*Document, error) {
	locales, err := i18n.Available()
	if err != nil {
		return nil, fmt.Errorf("list locales: %w", err)
	}
	sections := &Document{
		Schema:    SchemaURL,
		Generated: GeneratedMarker,
		SchemaID:  SchemaID,
		Project: Project{
			Status:     ProjectStatus,
			License:    ProjectLicense,
			Repository: ProjectRepository,
		},
		Checks:   buildChecks(config.CheckKinds()),
		NonGoals: NonGoals(),
	}
	for _, step := range []func() error{
		func() error { return requireFile(root, ContractDoc, "capabilities contract") },
		func() (serr error) { sections.Contracts, serr = buildContracts(root); return },
		func() (serr error) { sections.Adapters, serr = buildAdapters(root); return },
		func() (serr error) {
			sections.SandboxProviders, serr = buildProviders(root, registry.Descriptors())
			return
		},
		func() (serr error) {
			commands, cerr := buildCommands(root, cli.Commands())
			sections.CLI = CLI{Binary: CLIBinary, Commands: commands}
			return cerr
		},
		func() (serr error) {
			sections.Notifications, serr = buildNotifications(root, notify.Transports())
			return
		},
		func() (serr error) { sections.Locales, serr = buildLocales(root, locales); return },
	} {
		if serr := step(); serr != nil {
			return nil, serr
		}
	}
	return sections, nil
}

// Generate writes the rendered document to out.
func Generate(root, out string) error {
	doc, err := Build(root)
	if err != nil {
		return err
	}
	rendered, err := Render(doc)
	if err != nil {
		return err
	}
	// The committed file keeps whatever mode git gave it; the restrictive
	// mode here only applies if it is being created for the first time.
	if err := os.WriteFile(out, rendered, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", out, err)
	}
	return nil
}

func buildContracts(root string) (Contracts, error) {
	c := Contracts{
		AdapterProtocol: Contract{
			Version: adapter.ProtocolVersion,
			Spec:    "docs/adapter-protocol.md",
			Schema:  "docs/schemas/adapter/",
		},
		EvidenceSchema: EvidenceContract{
			Version:             evidence.SchemaID,
			ReadableVersions:    evidence.SchemaIDs(),
			Spec:                "docs/evidence-schema.md",
			Schema:              "docs/schemas/evidence/record.json",
			IndependentVerifier: "spec/evidence",
		},
		NotificationPayload: Contract{
			Version: notify.SchemaID,
			Spec:    "docs/notifications.md",
			Schema:  "docs/schemas/notification/payload.json",
		},
		// A push has no payload schema of its own: the bytes on the wire are
		// the evidence log, so the record schema is the body schema, one
		// record per line (docs/evidence-push.md §3).
		EvidencePush: Contract{
			Version: push.SchemaID,
			Spec:    "docs/evidence-push.md",
			Schema:  "docs/schemas/evidence/record.json",
		},
	}
	refs := []struct{ what, path string }{
		{"adapter protocol spec", c.AdapterProtocol.Spec},
		{"adapter protocol schemas", c.AdapterProtocol.Schema},
		{"evidence schema spec", c.EvidenceSchema.Spec},
		{"evidence record schema", c.EvidenceSchema.Schema},
		{"independent evidence verifier", c.EvidenceSchema.IndependentVerifier + "/"},
		{"notification spec", c.NotificationPayload.Spec},
		{"notification payload schema", c.NotificationPayload.Schema},
		{"evidence push spec", c.EvidencePush.Spec},
		{"evidence push body schema", c.EvidencePush.Schema},
	}
	for _, r := range refs {
		if err := requireFile(root, r.path, r.what); err != nil {
			return Contracts{}, err
		}
	}
	return c, nil
}

func buildAdapters(root string) ([]Adapter, error) {
	dirs, err := AdapterDirs(root)
	if err != nil {
		return nil, err
	}
	if len(dirs) == 0 {
		return nil, fmt.Errorf("no adapters found under %s", filepath.Join(root, "adapters"))
	}
	adapters := make([]Adapter, 0, len(dirs))
	for _, dir := range dirs {
		a, aerr := buildAdapter(root, dir)
		if aerr != nil {
			return nil, aerr
		}
		adapters = append(adapters, a)
	}
	return adapters, nil
}

func buildProviders(root string, descriptors []sandbox.Descriptor) ([]SandboxProvider, error) {
	providers := make([]SandboxProvider, 0, len(descriptors))
	for _, d := range descriptors {
		if !validStatus(d.Status) {
			return nil, fmt.Errorf("sandbox provider %s: status %q is not one of %s",
				d.ID, d.Status, strings.Join(Statuses(), ", "))
		}
		if err := requireFile(root, d.Docs, "sandbox provider "+d.ID+" docs"); err != nil {
			return nil, err
		}
		providers = append(providers, SandboxProvider{
			ID:              d.ID,
			Name:            d.Name,
			Status:          d.Status,
			Params:          buildParams(d.Params),
			Isolation:       buildIsolation(d.Isolation),
			Constraints:     d.Constraints,
			VerifiedAgainst: d.VerifiedAgainst,
			Docs:            nullable(d.Docs),
		})
	}
	return providers, nil
}

func buildParams(params []sandbox.Param) []ProviderParam {
	out := make([]ProviderParam, 0, len(params))
	for _, p := range params {
		out = append(out, ProviderParam{
			ID:       p.Key(),
			Required: p.Required,
			Default:  nullable(p.Default),
			Doc:      p.Doc,
		})
	}
	return out
}

func buildIsolation(iso sandbox.Isolation) Isolation {
	return Isolation{
		NetworkDefault:   nullable(iso.NetworkDefault),
		PublishedPorts:   iso.PublishedPorts,
		Storage:          iso.Storage,
		ForcedTeardown:   iso.ForcedTeardown,
		OrphanSweep:      iso.OrphanSweep,
		ExternalBackstop: nullable(iso.ExternalBackstop),
	}
}

func buildChecks(kinds []config.CheckKind) []Check {
	checks := make([]Check, 0, len(kinds))
	for _, k := range kinds {
		kind := CheckKindSQL
		if k.Builtin {
			kind = CheckKindBuiltin
		}
		params := make([]CheckParam, 0, len(k.Params))
		for _, p := range k.Params {
			params = append(params, CheckParam{
				ID:       p.Name,
				Type:     p.Type,
				Required: p.Required,
				Doc:      p.Doc,
			})
		}
		checks = append(checks, Check{
			ID:       k.ID,
			Name:     k.Name,
			Status:   k.Status,
			Kind:     kind,
			Params:   params,
			Requires: nullable(k.Requires),
		})
	}
	return checks
}

func buildCommands(root string, table []cli.Command) ([]Command, error) {
	commands := make([]Command, 0, len(table))
	for _, c := range table {
		if !validStatus(c.Status) {
			return nil, fmt.Errorf("command %q: status %q is not one of %s",
				c.ID, c.Status, strings.Join(Statuses(), ", "))
		}
		if err := requireFile(root, c.Docs, "command "+c.ID+" docs"); err != nil {
			return nil, err
		}
		flags := make([]Flag, 0, len(c.Flags))
		for _, f := range c.Flags {
			flags = append(flags, Flag{
				ID:         f.Name,
				Required:   f.Required,
				Repeatable: f.Repeatable,
				Doc:        f.Doc,
			})
		}
		exits := make([]ExitCode, 0, len(c.ExitCodes))
		for _, e := range c.ExitCodes {
			exits = append(exits, ExitCode{Code: e.Code, Meaning: e.Meaning})
		}
		commands = append(commands, Command{
			ID:         c.ID,
			Name:       CLIBinary + " " + c.ID,
			Status:     c.Status,
			Summary:    c.Summary,
			Flags:      flags,
			Positional: nullable(c.Positional),
			Stdout:     nullable(c.Stdout),
			ExitCodes:  exits,
			Docs:       nullable(c.Docs),
		})
	}
	return commands, nil
}

func buildNotifications(root string, transports []notify.Transport) (Notifications, error) {
	out := make([]Transport, 0, len(transports))
	for _, t := range transports {
		if !validStatus(t.Status) {
			return Notifications{}, fmt.Errorf("notification transport %s: status %q is not one of %s",
				t.ID, t.Status, strings.Join(Statuses(), ", "))
		}
		if err := requireFile(root, t.Docs, "notification transport "+t.ID+" docs"); err != nil {
			return Notifications{}, err
		}
		out = append(out, Transport{
			ID:          t.ID,
			Name:        t.Name,
			Status:      t.Status,
			Method:      t.Method,
			ContentType: t.ContentType,
			EventHeader: t.EventHeader,
			Signing: Signing{
				Algorithm: t.SignatureAlgorithm,
				Header:    t.SignatureHeader,
				Optional:  t.SigningOptional,
			},
			OutcomeFilter: config.NotifyOutcomes(),
			Delivery: Delivery{
				Attempts:              notify.Attempts,
				AttemptTimeoutSeconds: int(notify.AttemptTimeout.Seconds()),
				TotalBudgetSeconds:    int(notify.Budget.Seconds()),
			},
			Docs: nullable(t.Docs),
		})
	}
	return Notifications{
		PayloadVersion: notify.SchemaID,
		Event:          notify.Event,
		Transports:     out,
	}, nil
}

func buildLocales(root string, available []string) (Locales, error) {
	const docs = "docs/i18n.md"
	if err := requireFile(root, docs, "i18n spec"); err != nil {
		return Locales{}, err
	}
	return Locales{
		Source:    i18n.SourceLocale,
		Available: available,
		Scope:     i18n.TranslationScope,
		Docs:      nullable(docs),
	}, nil
}

// requireFile fails when a path the document points at is absent from the
// repository, so a moved or renamed document cannot leave a dead link in a
// published manifest. A trailing slash means the path must be a directory.
func requireFile(root, rel, what string) error {
	if rel == "" {
		return nil
	}
	wantDir := strings.HasSuffix(rel, "/")
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(strings.TrimSuffix(rel, "/"))))
	if err != nil {
		return fmt.Errorf("%s: %s is not in the repository", what, rel)
	}
	if info.IsDir() != wantDir {
		return fmt.Errorf("%s: %s is not a %s", what, rel, kindWord(wantDir))
	}
	return nil
}

func kindWord(dir bool) string {
	if dir {
		return "directory"
	}
	return "file"
}
