//go:build !windows

package main

import "os/exec"

func hideViteConsole(*exec.Cmd) {}
