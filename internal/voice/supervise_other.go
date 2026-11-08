//go:build !linux

package voice

import "os/exec"

func configureChild(cmd *exec.Cmd) {}
