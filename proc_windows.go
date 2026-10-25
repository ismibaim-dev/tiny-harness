//go:build windows

package main

import (
	"os/exec"
	"strconv"
)

func setupProc(cmd *exec.Cmd) {}

// taskkill /T takes the whole process tree down, not just the shell.
func killTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
}
