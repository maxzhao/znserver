package httpserver

import "os/exec"

func prepareExtension(cmd *exec.Cmd)            {}
func signalExtension(cmd *exec.Cmd, force bool) { _ = cmd.Process.Kill() }
