package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Text-mode version, used on systems without WebView2 (and for testing).

var in = bufio.NewReader(os.Stdin)

func runConsole() {
	defer func() {
		fmt.Print("\n  Press Enter to close...")
		in.ReadString('\n')
	}()
	fmt.Printf("\n  %s v%s\n  ----------------------------------------\n", appName, appVersion)

	ps, err := readProfiles()
	if err != nil {
		fmt.Println("\n  " + err.Error())
		return
	}
	profile := ps[0].Path
	if len(ps) > 1 {
		fmt.Println("\n  Thunderbird profiles:")
		for i, p := range ps {
			tag := ""
			if p.IsDefault {
				tag = "  (default)"
			}
			fmt.Printf("    %d) %s%s\n", i+1, p.Name, tag)
		}
		for {
			n, err := strconv.Atoi(ask(fmt.Sprintf("  Which profile? [1-%d, Enter = 1]: ", len(ps)), "1"))
			if err == nil && n >= 1 && n <= len(ps) {
				profile = ps[n-1].Path
				break
			}
		}
	}
	fmt.Printf("\n  Profile: %s\n\n    1) Install / update\n    2) Uninstall\n    3) Exit\n", profile)
	choice := ask("  Choose [1]: ", "1")
	if choice != "1" && choice != "2" {
		return
	}
	for thunderbirdRunning() {
		ask("\n  Close Thunderbird completely, then press Enter.", "")
	}
	report := func(id, status, msg string) {
		if status != "run" {
			fmt.Printf("  [%s] %s\n", status, msg)
		}
	}
	if choice == "2" {
		var remove []string
		if list := installedKnownAddons(profile); len(list) > 0 {
			fmt.Println("\n  These add-ons from the Fluentbird+ list are installed:")
			for _, a := range list {
				tag := ""
				if a.Ours {
					tag = "  (added by this installer)"
				}
				fmt.Printf("    - %s%s\n", a.Name, tag)
			}
			if strings.ToLower(ask("  Remove the ones this installer added? [Y/n]: ", "y")) == "y" {
				for _, a := range list {
					if a.Ours {
						remove = append(remove, a.GUID)
					}
				}
			}
		}
		_ = uninstallTheme(profile, remove, report)
		return
	}
	if _, err := installTheme(profile, report); err != nil {
		return
	}
	if slugs := chooseAddonsConsole(); len(slugs) > 0 {
		installAddons(profile, slugs, report)
		fmt.Println("\n  The new add-ons switch on the next time Thunderbird opens.")
	}
	if autoUpdateSupported() {
		if strings.ToLower(ask("\n  Keep Fluentbird+ up to date automatically? [Y/n]: ", "y")) == "y" {
			if err := enableAutoUpdate(profile); err != nil {
				fmt.Println("  [fail]", err)
			} else {
				fmt.Println("  [ok] Automatic updates on (checked once a day)")
			}
		} else {
			disableAutoUpdate(profile)
		}
	}
	fmt.Println("\n  Done. Open Thunderbird to see the new look.")
}

func chooseAddonsConsole() []string {
	sel := make([]bool, len(addons))
	for {
		fmt.Println("\n  Optional add-ons:")
		for i, a := range addons {
			box := "[ ]"
			if sel[i] {
				box = "[x]"
			}
			fmt.Printf("    %2d) %s %-22s %s\n", i+1, box, a.Name, a.Desc)
		}
		a := strings.ToLower(ask("\n  Numbers to tick (e.g. 1 3), A = all, N = none, Enter = continue: ", ""))
		switch a {
		case "":
			var out []string
			for i, s := range sel {
				if s {
					out = append(out, addons[i].Slug)
				}
			}
			return out
		case "a", "n":
			for i := range sel {
				sel[i] = a == "a"
			}
			continue
		}
		for _, f := range strings.FieldsFunc(a, func(r rune) bool { return r == ' ' || r == ',' }) {
			if n, err := strconv.Atoi(f); err == nil && n >= 1 && n <= len(addons) {
				sel[n-1] = !sel[n-1]
			}
		}
	}
}

func ask(prompt, def string) string {
	fmt.Print(prompt)
	line, _ := in.ReadString('\n')
	if line = strings.TrimSpace(line); line == "" {
		return def
	}
	return line
}
