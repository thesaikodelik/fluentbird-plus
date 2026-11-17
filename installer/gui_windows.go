//go:build windows

package main

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	webview2 "github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"
)

//go:embed ui/index.html
var indexHTML string

// runGUI shows the graphical installer. It returns false only when WebView2 is missing.
func runGUI() bool {
	width, height := windowSize(900, 640)
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		DataPath:  filepath.Join(os.TempDir(), "FluentbirdPlusSetup"),
		AutoFocus: true,
		WindowOptions: webview2.WindowOptions{
			Title:  "Fluentbird+ Setup",
			Width:  uint(width),
			Height: uint(height),
			IconId: 2,
			Center: true,
		},
	})
	if w == nil {
		msg, _ := windows.UTF16PtrFromString("This installer needs the Microsoft Edge WebView2 Runtime, which comes with Windows 11.\n\nInstall it from microsoft.com/edge/webview2, or run this file with --console for the text version.")
		title, _ := windows.UTF16PtrFromString("Fluentbird+ Setup")
		windows.MessageBox(0, msg, title, windows.MB_OK|windows.MB_ICONINFORMATION)
		return true
	}
	defer w.Destroy()
	w.SetSize(width, height, webview2.HintFixed)

	emit := func(v interface{}) {
		b, _ := json.Marshal(v)
		w.Dispatch(func() { w.Eval("window.fbEvent && window.fbEvent(" + string(b) + ")") })
	}
	report := func(id, status, msg string) {
		emit(map[string]string{"type": "step", "id": id, "status": status, "msg": msg})
	}
	var busy sync.Mutex

	w.Bind("fbInfo", func() map[string]interface{} {
		ps, err := readProfiles()
		res := map[string]interface{}{"version": appVersion, "addons": addons, "profiles": ps, "autoUpdate": autoUpdateSupported()}
		if err != nil {
			res["error"] = err.Error()
		}
		return res
	})
	w.Bind("fbRunning", func() bool { return thunderbirdRunning() })
	w.Bind("fbInstall", func(profile string, slugs []string, auto bool) bool {
		if !validProfile(profile) || !busy.TryLock() {
			return false
		}
		go func() {
			defer busy.Unlock()
			_, err := installTheme(profile, report)
			if err == nil && len(slugs) > 0 {
				installAddons(profile, slugs, report)
			}
			if err == nil && autoUpdateSupported() {
				if auto {
					report("updates", "run", "Turning on automatic updates")
					if e := enableAutoUpdate(profile); e != nil {
						report("updates", "fail", e.Error())
					} else {
						report("updates", "ok", "Automatic updates on (checked once a day)")
					}
				} else if autoUpdateOn(profile) {
					disableAutoUpdate(profile)
					report("updates", "ok", "Automatic updates turned off")
				}
			}
			done := map[string]interface{}{"type": "done", "ok": err == nil}
			if err != nil {
				done["error"] = err.Error()
			}
			emit(done)
		}()
		return true
	})
	w.Bind("fbProfileAddons", func(profile string) []InstalledAddon {
		if !validProfile(profile) {
			return nil
		}
		return installedKnownAddons(profile)
	})
	w.Bind("fbUninstall", func(profile string, guids []string) bool {
		if !validProfile(profile) || !busy.TryLock() {
			return false
		}
		go func() {
			defer busy.Unlock()
			err := uninstallTheme(profile, guids, report)
			done := map[string]interface{}{"type": "done", "ok": err == nil}
			if err != nil {
				done["error"] = err.Error()
			}
			emit(done)
		}()
		return true
	})
	w.Bind("fbOpenThunderbird", func() { openThunderbird() })
	w.Bind("fbClose", func() { w.Dispatch(func() { w.Terminate() }) })

	w.SetHtml(indexHTML)
	w.Run()
	return true
}

// windowSize scales the design size by the display scaling (125%, 150%...) and keeps it on screen.
func windowSize(w, h int) (int, int) {
	user32 := windows.NewLazySystemDLL("user32.dll")
	scale := 1.0
	if p := user32.NewProc("GetDpiForSystem"); p.Find() == nil {
		if dpi, _, _ := p.Call(); dpi > 0 {
			scale = float64(dpi) / 96
		}
	}
	sw, sh := w*4, h*4
	if p := user32.NewProc("GetSystemMetrics"); p.Find() == nil {
		x, _, _ := p.Call(0) // SM_CXSCREEN
		y, _, _ := p.Call(1) // SM_CYSCREEN
		if x > 0 && y > 0 {
			sw, sh = int(x), int(y)
		}
	}
	fw, fh := int(float64(w)*scale), int(float64(h)*scale)
	if fw > sw*92/100 {
		fw = sw * 92 / 100
	}
	if fh > sh*88/100 {
		fh = sh * 88 / 100
	}
	return fw, fh
}
