package config

import (
	"fmt"
	"regexp"
	"strings"
)

// identifier.go owns the rule for what a drill may name: the shape of the
// table and column parameters (drill-config.md §3.5), and of a backup
// manifest's baseline keys, which name the same things (§2.1 of
// backup-manifest.md).
//
// The rule lives here, in one place, because two packages enforce it for
// different reasons and must not drift. internal/checks quotes an
// identifier before it reaches an engine, so the rule is what makes
// injection impossible: a part that cannot contain a quoting character
// cannot end a quoted one, whatever quoting an adapter declared.
// internal/manifest refuses a baseline key outside it, so a name no check
// could ever run is refused where it is written rather than where the
// reconciliation would have failed.

// identifierPart is what one segment of an identifier may be. It is
// deliberately narrower than what most engines accept: this is the set
// that needs no escaping anywhere, and widening it is a decision about
// every adapter at once.
var identifierPart = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// maxIdentifierParts is the qualification depth: name, or schema.name.
// A third part would be a database or catalogue, which a drill addresses
// through the adapter rather than through a check parameter.
const maxIdentifierParts = 2

// IdentifierParts validates a possibly qualified identifier and returns
// its parts, unquoted and in order.
func IdentifierParts(name string) ([]string, error) {
	parts := strings.Split(name, ".")
	if len(parts) > maxIdentifierParts {
		return nil, fmt.Errorf("invalid identifier %s: at most schema.name", name)
	}
	for _, part := range parts {
		if !identifierPart.MatchString(part) {
			return nil, fmt.Errorf("invalid identifier: %s", name)
		}
	}
	return parts, nil
}
