//go:build !linux

package voice

import "os/exec"

func configureChild(_ *exec.Cmd) {}
