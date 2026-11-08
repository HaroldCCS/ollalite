//go:build windows

package main

import (
	"context"
	"os/exec"
	"strconv"
)

func detach(cmd *exec.Cmd) {}

func killTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
}

func shellCmd(ctx context.Context, command string) *exec.Cmd {
	return exec.CommandContext(ctx, "cmd", "/C", command)
}

func shellName() string { return "cmd" }

func rssMB() float64 { return 0 }
