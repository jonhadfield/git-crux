//go:build !unix

package main

import "os/exec"

func setDetached(cmd *exec.Cmd) {}
