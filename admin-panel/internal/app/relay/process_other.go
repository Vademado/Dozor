//go:build !windows

package relay

import "os/exec"

func hideWindow(*exec.Cmd) {}
