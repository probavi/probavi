package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/probavi/probavi/internal/i18n"
)

// GameDay is a multi-database restore exercise: member drills executed in
// dependency order (docs/gameday.md). Hash and Path are filled by
// LoadGameDay, never by YAML.
type GameDay struct {
	Name        string          `yaml:"name"`
	Timeout     Duration        `yaml:"timeout"`
	MaxParallel int             `yaml:"max_parallel"`
	Members     []GameDayMember `yaml:"members"`

	// Hash is "sha256:<hex>" over the exact file bytes as read, reported
	// in the game-day summary.
	Hash string `yaml:"-"`
	// Path is the config file path LoadGameDay read, for error messages.
	Path string `yaml:"-"`
}

// GameDayMember references one member drill. Config is a drill
// configuration path, resolved relative to the game-day file's directory
// by LoadGameDay; DependsOn names members whose drills must pass first.
type GameDayMember struct {
	Name      string   `yaml:"name"`
	Config    string   `yaml:"config"`
	DependsOn []string `yaml:"depends_on"`
}

// Parallelism returns the effective member concurrency: max_parallel,
// defaulting to 1 (strictly sequential).
func (g *GameDay) Parallelism() int {
	if g.MaxParallel < 1 {
		return 1
	}
	return g.MaxParallel
}

// LoadGameDay reads, parses, and validates a game-day configuration,
// including every member drill configuration — a game-day fails fast on
// any problem before a single sandbox exists. Diagnostics speak the
// translator's language (docs/i18n.md).
func LoadGameDay(path string, tr *i18n.T) (*GameDay, error) {
	if tr == nil {
		tr = i18n.English()
	}
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, errorf(tr, msgReadGameDay, err)
	}
	g := &GameDay{}
	dec := yaml.NewDecoder(bytes.NewReader(raw), yaml.Strict())
	if err := dec.Decode(g); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errorf(tr, msgGameDayEmpty, path)
		}
		return nil, errorf(tr, msgParseGameDay, path, yaml.FormatError(err, false, true))
	}
	sum := sha256.Sum256(raw)
	g.Hash = "sha256:" + hex.EncodeToString(sum[:])
	g.Path = path
	if err := g.validate(tr); err != nil {
		return nil, errorf(tr, msgInvalidGameDay, path, err)
	}
	if err := g.loadMembers(filepath.Dir(path), tr); err != nil {
		return nil, errorf(tr, msgInvalidGameDay, path, err)
	}
	return g, nil
}

// loadMembers resolves member config paths against the game-day file's
// directory and loads each one, so a broken member surfaces before the
// exercise starts. It also enforces the shared-evidence-log rule: with
// max_parallel above 1, two members writing one log would collide on the
// store's single-writer lock mid-exercise — reject the combination now.
func (g *GameDay) loadMembers(base string, tr *i18n.T) error {
	p := problems{tr: tr}
	logOwner := map[string]string{}
	for i := range g.Members {
		m := &g.Members[i]
		if !filepath.IsAbs(m.Config) {
			m.Config = filepath.Join(base, m.Config)
		}
		cfg, err := Load(m.Config, tr)
		if err != nil {
			p.add("members[%d] (%s): %v", i, m.Name, err)
			continue
		}
		if g.Parallelism() > 1 {
			key := evidenceLogKey(cfg.Evidence.Path)
			if first, taken := logOwner[key]; taken {
				p.add(msgSharedEvidenceLog, first, m.Name, cfg.Evidence.Path, g.MaxParallel)
			} else {
				logOwner[key] = m.Name
			}
		}
	}
	return errors.Join(p.errs...)
}

// evidenceLogKey is what decides whether two members name one log.
//
// The guard exists to move a collision from the store's single-writer
// lock, mid-exercise, to config load — so it has to compare files rather
// than spellings: `/var/lib/evidence.jsonl` and `/var/lib/./evidence.jsonl`
// are one file and would otherwise both start.
//
// filepath.Abs is the right resolution and not merely a tidy-up, because
// it is the same one the log will get: an evidence path is never resolved
// against the drill file's directory, it is handed to evidence.Open as
// written, and every member of a game-day runs in one process. So the
// working directory this reads is the working directory that will open
// the file. Abs fails only when that directory cannot be determined, and
// cleaning alone is still better than nothing there.
//
// A symlinked directory still slips through. Following those would mean
// touching the filesystem to validate a config, which nothing else in
// this package does, and the collision it hides fails safely — at the
// lock, with the diagnostic that says so.
func evidenceLogKey(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return abs
}

func (g *GameDay) validate(tr *i18n.T) error {
	p := problems{tr: tr}
	if g.Name == "" {
		p.add(msgGameDayNameRequired)
	}
	if g.Timeout == 0 {
		p.add(msgGameDayTimeoutRequired)
	}
	if g.MaxParallel < 0 {
		p.add(msgMaxParallelNegative)
	}
	if len(g.Members) == 0 {
		p.add(msgMembersRequired)
		return errors.Join(p.errs...)
	}
	index := g.validateMembers(&p)
	g.validateDependencies(&p, index)
	if len(p.errs) == 0 {
		if stuck := g.cycleMembers(); len(stuck) > 0 {
			p.add(msgDependencyCycle, strings.Join(stuck, ", "))
		}
	}
	return errors.Join(p.errs...)
}

// validateMembers checks per-member shape and returns the name index used
// for dependency resolution.
func (g *GameDay) validateMembers(p *problems) map[string]int {
	index := make(map[string]int, len(g.Members))
	for i := range g.Members {
		m := &g.Members[i]
		switch {
		case m.Name == "":
			p.add(msgMemberNameRequired, i)
		default:
			if prev, dup := index[m.Name]; dup {
				p.add(msgMemberNameDuplicate, i, m.Name, prev)
			} else {
				index[m.Name] = i
			}
		}
		if m.Config == "" {
			p.add(msgMemberConfigRequired, i)
		}
		seen := make(map[string]bool, len(m.DependsOn))
		for _, dep := range m.DependsOn {
			switch {
			case dep == m.Name:
				p.add(msgMemberSelfDependency, i)
			case seen[dep]:
				p.add(msgMemberDuplicateDep, i, dep)
			}
			seen[dep] = true
		}
	}
	return index
}

func (g *GameDay) validateDependencies(p *problems, index map[string]int) {
	for i := range g.Members {
		m := &g.Members[i]
		for _, dep := range m.DependsOn {
			if _, ok := index[dep]; !ok && dep != m.Name {
				p.add(msgMemberUnknownDep, i, dep)
			}
		}
	}
}

// cycleMembers runs Kahn's algorithm over the dependency graph; members
// whose in-degree never reaches zero sit in or behind a cycle.
func (g *GameDay) cycleMembers() []string {
	indegree := make(map[string]int, len(g.Members))
	dependents := map[string][]string{}
	for i := range g.Members {
		m := &g.Members[i]
		indegree[m.Name] += 0
		for _, dep := range m.DependsOn {
			indegree[m.Name]++
			dependents[dep] = append(dependents[dep], m.Name)
		}
	}
	var queue []string
	for i := range g.Members {
		if indegree[g.Members[i].Name] == 0 {
			queue = append(queue, g.Members[i].Name)
		}
	}
	removed := 0
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		removed++
		for _, d := range dependents[name] {
			indegree[d]--
			if indegree[d] == 0 {
				queue = append(queue, d)
			}
		}
	}
	if removed == len(g.Members) {
		return nil
	}
	var stuck []string
	for i := range g.Members {
		if indegree[g.Members[i].Name] > 0 {
			stuck = append(stuck, g.Members[i].Name)
		}
	}
	return stuck
}
