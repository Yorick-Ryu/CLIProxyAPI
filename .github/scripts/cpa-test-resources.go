//go:build linux

// cpa-test-resources records process resource usage without requiring GNU time
// in the target CPA container. It runs only explicit test commands.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: resources result.json command [args...]")
		os.Exit(2)
	}
	cmd := exec.Command(os.Args[2], os.Args[3:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)
	code := 0
	if err != nil {
		code = 1
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		}
	}
	result := map[string]any{"elapsed_seconds": elapsed.Seconds(), "exit_code": code}
	if cmd.ProcessState != nil {
		result["user_seconds"] = cmd.ProcessState.UserTime().Seconds()
		result["system_seconds"] = cmd.ProcessState.SystemTime().Seconds()
		if usage, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
			result["max_rss_kib"] = usage.Maxrss
		}
	}
	raw, marshalErr := json.MarshalIndent(result, "", "  ")
	if marshalErr != nil {
		fmt.Fprintln(os.Stderr, marshalErr)
		os.Exit(2)
	}
	if writeErr := os.WriteFile(os.Args[1], append(raw, '\n'), 0600); writeErr != nil {
		fmt.Fprintln(os.Stderr, writeErr)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(code)
}
