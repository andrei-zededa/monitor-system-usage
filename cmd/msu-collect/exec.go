package main

import (
	"os"
	"os/exec"
	"strings"
)

// readFile reads a file and returns its contents as a string.
// Returns the content and any error.
func readFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(data), "\n"), nil
}

// runCmd executes a command and returns its stdout as a string.
func runCmd(args []string) (string, error) {
	cmd := exec.Command(args[0], args[1:]...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// readFileInNS reads a file inside a network namespace.
func readFileInNS(ns, path string) (string, error) {
	return runCmd([]string{"ip", "netns", "exec", ns, "cat", path})
}

// runCmdInNS executes a command inside a network namespace.
func runCmdInNS(ns string, args []string) (string, error) {
	fullArgs := append([]string{"ip", "netns", "exec", ns}, args...)
	return runCmd(fullArgs)
}
