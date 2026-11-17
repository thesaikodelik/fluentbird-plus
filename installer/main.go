// Fluentbird+ Installer
//
// Installs the Fluentbird+ theme (a polish layer on top of Fluentbird by Danny King)
// into a Thunderbird profile, and optionally downloads a curated set of add-ons
// from the official Thunderbird add-ons site (addons.thunderbird.net).
//
// What it touches, and nothing else:
//   - <profile>\chrome\            theme files (an existing folder is backed up first)
//   - <profile>\user.js            the settings the theme needs (existing file is backed up)
//   - <profile>\extensions\*.xpi   only the add-ons you tick
//
// Network: the only server contacted is addons.thunderbird.net, and only if you pick add-ons.
// No telemetry, no accounts, no personal data read or sent.
package main

import "os"

func main() {
	if testAPI != "" {
		githubAPI, githubWeb = testAPI, testAPI
	}
	for _, a := range os.Args[1:] {
		switch a {
		case "--console", "-c":
			runConsole()
			return
		case "--auto-update": // run by the daily scheduled task, no window
			runAutoUpdate()
			return
		case "--apply-update":
			runApplyUpdate()
			return
		case "--policy-add", "--policy-remove": // the elevated helper step
			os.Exit(runPolicyCommand(os.Args[1:]))
		}
	}
	if !runGUI() {
		runConsole()
	}
}
