// Package docs holds the gates that keep this repository honest about
// itself. It contains no production code: every file in it is a test, and
// what each one tests is a claim the repository makes somewhere a compiler
// never looks.
//
// The oldest of them, and the reason for the name, pins the translated
// README files to the English original (docs/i18n.md §7): the suite hashes
// the marked spans of README.md and holds every README.<tag>.md to the hash
// it records, so an edit to the English source cannot ship without the
// translations being refreshed. English is the canonical source language
// (AGENTS.md §5.7); translations are derived artifacts, and a translation
// may only exist here while a machine can prove it is current.
//
// The rest grew from the same principle applied to everything else the
// repository asserts outside its own code — the release workflow's assets,
// the packaging recipes, the container image, the engine tables and badges,
// the ignore list, the version matrix's one requirable check, the versions
// named in install instructions, and the refusal to carry key material.
// They are collected here rather than beside what they check because none
// of them belongs to a package: they are claims about the tree.
package docs
