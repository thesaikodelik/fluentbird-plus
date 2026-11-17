//go:build !windows

package main

import "os/exec"

func hideWindow(cmd *exec.Cmd) {}

// runGUI is Windows only; other systems use the text installer.
func runGUI() bool { return false }

func registerTask(exe string) error { return nil }
func unregisterTask()               {}

func policySupported() bool                 { return false }
func addonCacheDir() string                 { return "" }
func queuePolicyInstall(files []string) error { return nil }
func removePolicyInstalls() error           { return nil }
func runPolicyCommand(args []string) int    { return 2 }
