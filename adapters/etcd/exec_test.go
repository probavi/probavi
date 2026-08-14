package main

import (
	"os/exec"
)

// execCommand runs argv directly, the way the sandbox exec verb does: no
// shell wrapper of the harness's own. Test-only helper.
func execCommand(argv []string) error {
	return exec.Command(argv[0], argv[1:]...).Run()
}
