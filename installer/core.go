package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	theme "github.com/thesaikodelik/fluentbird-plus"
)

const (
	appName    = "Fluentbird+ Installer"
	markerFile = ".fluentbird-plus"
	blockStart = "// >>> Fluentbird+ (added by Fluentbird+ Installer) >>>"
	blockEnd   = "// <<< Fluentbird+ <<<"
	apiBase    = "https://addons.thunderbird.net/api/v4/addons/addon/"
)

// Set at build time by the release workflow:
//   -ldflags "-X main.appVersion=1.3.0 -X main.updateRepo=owner/repo"
// When updateRepo is empty, automatic updates are not offered.
var (
	appVersion = "1.4.3"
	updateRepo = ""
	testAPI    = "" // build-time override used only by tests
)

func userAgent() string { return "FluentbirdPlus-Installer/" + appVersion }

var themeFS = theme.Files

var prefs = []struct{ key, value string }{
	{"toolkit.legacyUserProfileCustomizations.stylesheets", "true"},
	{"widget.windows.mica", "true"},
	{"widget.windows.mica.popups", "2"},
	// Add-ons placed in the profile folder start switched off until the user finds and
	// enables them in the Add-ons Manager. 14 keeps that protection for every other
	// location, but lets the add-ons you tick in the installer turn on by themselves.
	// Uninstall puts the old value back.
	{"extensions.autoDisableScopes", "14"},
}

type Addon struct {
	Slug   string `json:"slug"`
	GUID   string `json:"guid"`
	Name   string `json:"name"`
	Desc   string `json:"desc"`
	Manual bool   `json:"manual"` // uses Experiment APIs: Thunderbird only allows installing these from inside Thunderbird
}

var addons = []Addon{
	{"quickfilters", "quickFilters@axelg.com", "quickFilters", "Drag an email to a folder to create a filter.", false},
	{"undo-send-notification", "undo_send@thanasis-taprantzis", "Undo Send", "A few seconds to cancel an email after Send.", false},
	{"check-and-send", "{1B0ADFEC-846C-401D-BA54-7842CBD485D4}", "Check and Send", "Confirms recipients and account before sending.", false},
	{"dkim-verifier", "dkim_verifier@pl", "DKIM Verifier", "Shows whether the sender's signature is valid.", false},
	{"extract-em", "extractem@tmccoid.tech", "Extract 'Em!", "Save every attachment in a folder at once.", false},
	{"auto-profile-picture", "autoprofilepicture@astucesweb.fr", "Auto Profile Picture", "Shows sender pictures and logos next to emails.", true},
	{"send-later-3", "sendlater3@kamens.us", "Send Later", "Write now, send at the time you choose.", true},
	{"xnotepp", "xnote@froihofer.net", "XNote++", "Private sticky notes on any email.", true},
	{"quick-folder-move", "quickmove@mozilla.kewis.ch", "Quick Folder Move", "File an email by typing part of a folder name.", true},
	{"mail-merge", "mailmerge@example.net", "Mail Merge", "Personalised emails to a list of people.", true},
	{"expression-search-ng", "expressionsearch@opto.one", "Expression Search NG", "Powerful search, like f:name or has attachment.", true},
}

// Progress reports a step: status is "run", "ok", "skip" or "fail".
type Progress func(id, status, msg string)

// ---------- profile detection ----------

type Profile struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	IsDefault bool   `json:"isDefault"`
	Installed bool   `json:"installed"`
	Accounts  int    `json:"accounts"`
	LastUsed  string `json:"lastUsed"`
}

// profileSummary counts email identities and reads when the profile was last used.
// Only counts are read; no addresses leave this function.
func profileSummary(dir string) (int, string) {
	path := filepath.Join(dir, "prefs.js")
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, ""
	}
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		k := prefKey(strings.TrimSpace(line))
		if strings.HasPrefix(k, "mail.identity.") && strings.HasSuffix(k, ".useremail") {
			n++
		}
	}
	last := ""
	if fi, err := os.Stat(path); err == nil {
		last = fi.ModTime().Format("2 Jan 2006")
	}
	return n, last
}

func thunderbirdRoot() (string, error) {
	if runtime.GOOS == "windows" {
		appdata := os.Getenv("APPDATA")
		if appdata == "" {
			return "", errors.New("APPDATA is not set")
		}
		return filepath.Join(appdata, "Thunderbird"), nil
	}
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Thunderbird"), nil
	}
	return filepath.Join(home, ".thunderbird"), nil
}

func readProfiles() ([]Profile, error) {
	root, err := thunderbirdRoot()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(root, "profiles.ini"))
	if err != nil {
		return nil, errors.New("Thunderbird was not found. Install it and open it once, then run this installer again.")
	}
	sections := map[string]map[string]string{}
	var order []string
	cur := ""
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r", ""), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			cur = line[1 : len(line)-1]
			sections[cur] = map[string]string{}
			order = append(order, cur)
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok && cur != "" {
			sections[cur][strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	installDefault := ""
	for _, name := range order {
		if strings.HasPrefix(name, "Install") {
			installDefault = sections[name]["Default"]
		}
	}
	var out []Profile
	for _, name := range order {
		s := sections[name]
		if !strings.HasPrefix(name, "Profile") || s["Path"] == "" {
			continue
		}
		p := s["Path"]
		full := p
		if s["IsRelative"] == "1" {
			full = filepath.Join(root, filepath.FromSlash(p))
		}
		def := s["Default"] == "1"
		if installDefault != "" {
			def = p == installDefault
		}
		if fi, err := os.Stat(full); err == nil && fi.IsDir() {
			n, last := profileSummary(full)
			out = append(out, Profile{Name: s["Name"], Path: full, IsDefault: def, Installed: isInstalled(full), Accounts: n, LastUsed: last})
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no Thunderbird profiles were found")
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].IsDefault && !out[j].IsDefault })
	return out, nil
}

// validProfile makes sure a path received from the UI is one of the real profiles.
func validProfile(path string) bool {
	ps, err := readProfiles()
	if err != nil {
		return false
	}
	for _, p := range ps {
		if p.Path == path {
			return true
		}
	}
	return false
}

// ---------- Thunderbird process ----------

func thunderbirdRunning() bool {
	if runtime.GOOS != "windows" {
		return false
	}
	cmd := exec.Command("tasklist", "/FI", "IMAGENAME eq thunderbird.exe", "/NH")
	hideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(out)), "thunderbird.exe")
}

func openThunderbird() {
	if runtime.GOOS != "windows" {
		return
	}
	cmd := exec.Command("cmd", "/c", "start", "", "thunderbird")
	hideWindow(cmd)
	_ = cmd.Start()
}

// ---------- install / uninstall ----------
//
// Everything needed to undo an install lives in <profile>\fluentbird-plus-backup\:
//   state.json  original values of the theme settings, whether user.js existed,
//               whether a chrome folder was saved, and which add-ons we added
//   chrome\     the user's own chrome folder, if they had one
//   user.js     the user's original user.js, if they had one
// Uninstall puts all of this back and then deletes the backup folder.

const backupDirName = "fluentbird-plus-backup"

type prefState struct {
	Set   bool   `json:"set"`
	Value string `json:"value,omitempty"`
}

type installState struct {
	Version       string               `json:"version"`
	Created       string               `json:"created"`
	Prefs         map[string]prefState `json:"prefs"`
	UserJSExisted bool                 `json:"userJsExisted"`
	ChromeSaved   bool                 `json:"chromeSaved"`
	Addons        []string             `json:"addons"`
}

func backupDir(profile string) string { return filepath.Join(profile, backupDirName) }

func loadState(profile string) (*installState, error) {
	b, err := os.ReadFile(filepath.Join(backupDir(profile), "state.json"))
	if err != nil {
		return nil, err
	}
	var st installState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	if st.Prefs == nil {
		st.Prefs = map[string]prefState{}
	}
	return &st, nil
}

func saveState(profile string, st *installState) error {
	b, _ := json.MarshalIndent(st, "", "  ")
	return os.WriteFile(filepath.Join(backupDir(profile), "state.json"), b, 0o644)
}

// isFluentbirdChrome reports whether a chrome folder holds Fluentbird (ours, or a manual copy).
func isFluentbirdChrome(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, markerFile)); err == nil {
		return true
	}
	b, err := os.ReadFile(filepath.Join(dir, "userChrome.css"))
	return err == nil && strings.Contains(strings.ToUpper(string(b[:min(len(b), 600)])), "FLUENTBIRD")
}

func isInstalled(profile string) bool {
	if _, err := os.Stat(filepath.Join(backupDir(profile), "state.json")); err == nil {
		return true
	}
	return isFluentbirdChrome(filepath.Join(profile, "chrome"))
}

func installTheme(profile string, report Progress) (*installState, error) {
	chrome := filepath.Join(profile, "chrome")
	bdir := backupDir(profile)

	st, err := loadState(profile)
	if err != nil {
		// First install: remember how things were.
		report("backup", "run", "Saving your current settings")
		if err := os.MkdirAll(bdir, 0o755); err != nil {
			report("backup", "fail", "Could not create the backup folder")
			return nil, err
		}
		st = &installState{Version: appVersion, Created: time.Now().Format(time.RFC3339), Prefs: map[string]prefState{}}
		// If Fluentbird was already set up by hand, its settings are not "the user's own":
		// uninstall should go back to Thunderbird's defaults for them.
		manual := isFluentbirdChrome(chrome)
		cur := readPrefsJS(profile)
		for _, p := range prefs {
			v, ok := cur[p.key]
			if manual {
				ok, v = false, ""
			}
			st.Prefs[p.key] = prefState{Set: ok, Value: v}
		}
		if b, err := os.ReadFile(filepath.Join(profile, "user.js")); err == nil {
			orig := string(b) // keep the user's file byte for byte
			if manual {
				orig = strings.TrimSpace(stripOurLines(orig, true))
			}
			st.UserJSExisted = strings.TrimSpace(orig) != "" || !manual
			if st.UserJSExisted {
				if err := os.WriteFile(filepath.Join(bdir, "user.js"), []byte(orig), 0o644); err != nil {
					report("backup", "fail", "Could not back up user.js")
					return nil, err
				}
			}
		}
		if fi, err := os.Stat(chrome); err == nil && fi.IsDir() && !isFluentbirdChrome(chrome) {
			if err := os.Rename(chrome, filepath.Join(bdir, "chrome")); err != nil {
				report("backup", "fail", "Could not back up your chrome folder")
				return nil, err
			}
			st.ChromeSaved = true
		}
		if err := saveState(profile, st); err != nil {
			report("backup", "fail", "Could not save the backup information")
			return nil, err
		}
		report("backup", "ok", "Your current settings are saved, so uninstall can restore them")
	}

	report("theme", "run", "Copying theme files")
	if _, err := os.Stat(chrome); err == nil {
		if !isFluentbirdChrome(chrome) {
			// A chrome folder appeared after our install. Keep it safe rather than delete it.
			_ = os.Rename(chrome, chrome+".old-"+time.Now().Format("20060102-150405"))
		} else if err := os.RemoveAll(chrome); err != nil {
			report("theme", "fail", "Could not replace the old theme files")
			return nil, err
		}
	}
	err = fs.WalkDir(themeFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dst := filepath.Join(chrome, filepath.FromSlash(p))
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := themeFS.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
	if err != nil {
		report("theme", "fail", "Could not copy the theme files")
		return nil, err
	}
	_ = os.WriteFile(filepath.Join(chrome, markerFile), []byte("Installed by "+appName+" v"+appVersion+"\n"), 0o644)
	report("theme", "ok", "Theme files installed")

	report("prefs", "run", "Turning on theme settings")
	if err := setUserJSBlock(profile, true); err != nil {
		report("prefs", "fail", "Could not update user.js")
		return nil, err
	}
	report("prefs", "ok", "Theme settings turned on")
	return st, nil
}

// uninstallTheme returns Thunderbird to how it was before the first install.
// Without saved state (older or manual installs) it returns to Thunderbird's defaults.
func uninstallTheme(profile string, removeGUIDs []string, report Progress) error {
	chrome := filepath.Join(profile, "chrome")
	bdir := backupDir(profile)
	st, stErr := loadState(profile)

	// 1. Theme files
	report("theme", "run", "Removing theme files")
	if fi, err := os.Stat(chrome); err == nil && fi.IsDir() {
		if isFluentbirdChrome(chrome) {
			if err := os.RemoveAll(chrome); err != nil {
				report("theme", "fail", "Could not remove the theme files")
				return err
			}
			report("theme", "ok", "Theme files removed")
		} else {
			report("theme", "skip", "Your chrome folder is not Fluentbird, so it was left alone")
		}
	} else {
		report("theme", "skip", "No theme files found")
	}
	// Leftover backups from version 1.0.0 that are only old copies of Fluentbird.
	olds, _ := filepath.Glob(chrome + ".backup-*")
	sort.Strings(olds)
	var userOld string
	for _, o := range olds {
		if isFluentbirdChrome(o) {
			_ = os.RemoveAll(o)
		} else {
			userOld = o
		}
	}

	// 2. The user's own chrome folder
	restoredChrome := false
	if st != nil && st.ChromeSaved {
		if _, err := os.Stat(chrome); err != nil {
			if err := os.Rename(filepath.Join(bdir, "chrome"), chrome); err == nil {
				restoredChrome = true
			}
		}
	} else if userOld != "" {
		if _, err := os.Stat(chrome); err != nil {
			if err := os.Rename(userOld, chrome); err == nil {
				restoredChrome = true
			}
		}
	}
	if restoredChrome {
		report("chrome", "ok", "Your own chrome folder was put back")
	}

	// 3. Settings
	report("prefs", "run", "Restoring settings")
	if err := restoreUserJS(profile, st); err != nil {
		report("prefs", "fail", "Could not restore user.js")
		return err
	}
	target := map[string]prefState{}
	for _, p := range prefs {
		if st != nil {
			target[p.key] = st.Prefs[p.key]
		} else {
			target[p.key] = prefState{} // Thunderbird default
		}
	}
	if st == nil && restoredChrome {
		// A non-Fluentbird theme came back, and it needs custom styles enabled.
		target["toolkit.legacyUserProfileCustomizations.stylesheets"] = prefState{Set: true, Value: "true"}
	}
	if err := writePrefsJS(profile, target); err != nil {
		report("prefs", "fail", "Could not restore prefs.js")
		return err
	}
	if olds, _ := filepath.Glob(filepath.Join(profile, "user.js.backup-*")); len(olds) > 0 {
		for _, o := range olds {
			if b, err := os.ReadFile(o); err == nil && strings.TrimSpace(stripOurLines(string(b), true)) == "" {
				_ = os.Remove(o) // only held our own settings
			}
		}
	}
	if st != nil {
		report("prefs", "ok", "Settings restored to how they were before")
	} else {
		report("prefs", "ok", "Settings reset to Thunderbird defaults")
	}

	// 4. Add-ons the user chose to remove
	if len(removeGUIDs) > 0 {
		dir := filepath.Join(profile, "extensions")
		for _, g := range removeGUIDs {
			a, ok := findAddonByGUID(g)
			if !ok {
				continue // only add-ons from our own list
			}
			id := "addon:" + a.Slug
			f := filepath.Join(dir, a.GUID+".xpi")
			if _, err := os.Stat(f); err != nil {
				continue
			}
			if err := os.Remove(f); err != nil {
				report(id, "fail", "Could not remove "+a.Name)
			} else {
				report(id, "ok", a.Name+" removed")
			}
		}
	}

	// 5. Remove the add-on install entries from Windows (asks for admin once)
	if policySupported() {
		if err := removePolicyInstalls(); err != nil {
			report("policy", "skip", "Add-on install entries were left in Windows: "+err.Error())
		}
		_ = os.RemoveAll(addonCacheDir())
		removePrefs(profile, policyRunOncePref)
	}

	// 5. Stop automatic updates for this profile
	if autoUpdateOn(profile) {
		disableAutoUpdate(profile)
		report("updates", "ok", "Automatic updates turned off")
	}

	// 6. Clean up our backup folder
	if stErr == nil {
		if err := os.RemoveAll(bdir); err != nil {
			report("cleanup", "skip", "The backup folder could not be removed: "+backupDirName)
		}
	}
	report("done", "ok", "Thunderbird is back to how it was")
	return nil
}

// setUserJSBlock writes our settings block into user.js, keeping the user's own lines.
func setUserJSBlock(profile string, add bool) error {
	path := filepath.Join(profile, "user.js")
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	kept := stripOurLines(string(old), true)
	if !add && strings.TrimSpace(kept) == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	var b strings.Builder
	if strings.TrimSpace(kept) != "" {
		b.WriteString(strings.TrimRight(kept, "\r\n") + "\r\n\r\n")
	}
	if add {
		b.WriteString(blockStart + "\r\n")
		for _, p := range prefs {
			fmt.Fprintf(&b, "user_pref(%q, %s);\r\n", p.key, p.value)
		}
		b.WriteString(blockEnd + "\r\n")
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// stripOurLines removes our block, and (when installing) any loose copies of our keys.
func stripOurLines(text string, dropOurKeys bool) string {
	var kept []string
	inBlock := false
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		switch {
		case t == blockStart:
			inBlock = true
			continue
		case t == blockEnd:
			inBlock = false
			continue
		case inBlock:
			continue
		}
		if dropOurKeys && isOurKey(prefKey(t)) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\r\n")
}

func restoreUserJS(profile string, st *installState) error {
	path := filepath.Join(profile, "user.js")
	if st == nil {
		return setUserJSBlock(profile, false)
	}
	if st.UserJSExisted {
		b, err := os.ReadFile(filepath.Join(backupDir(profile), "user.js"))
		if err != nil {
			return setUserJSBlock(profile, false)
		}
		return os.WriteFile(path, b, 0o644)
	}
	// There was no user.js before. Keep anything the user added since, without our block.
	return setUserJSBlock(profile, false)
}

// ---------- prefs.js (Thunderbird must be closed) ----------

func prefKey(line string) string {
	if !strings.HasPrefix(line, "user_pref(\"") {
		return ""
	}
	rest := line[len("user_pref(\""):]
	if i := strings.Index(rest, "\""); i > 0 {
		return rest[:i]
	}
	return ""
}

func prefValue(line string) string {
	i := strings.Index(line, "\",")
	j := strings.LastIndex(line, ");")
	if i < 0 || j <= i {
		return ""
	}
	return strings.TrimSpace(line[i+2 : j])
}

func isOurKey(k string) bool {
	for _, p := range prefs {
		if p.key == k {
			return true
		}
	}
	return false
}

func readPrefsJS(profile string) map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(filepath.Join(profile, "prefs.js"))
	if err != nil {
		return out
	}
	for _, line := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		if k := prefKey(t); k != "" && isOurKey(k) {
			out[k] = prefValue(t)
		}
	}
	return out
}

// writePrefsJS sets our keys to the target state: removed (Thunderbird default) or a value.
func writePrefsJS(profile string, target map[string]prefState) error {
	path := filepath.Join(profile, "prefs.js")
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	nl := "\n"
	if strings.Contains(string(b), "\r\n") {
		nl = "\r\n"
	}
	written := map[string]bool{}
	var out []string
	for _, line := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		if k := prefKey(strings.TrimSpace(line)); isOurKey(k) {
			if t := target[k]; t.Set && !written[k] {
				out = append(out, fmt.Sprintf("user_pref(%q, %s);", k, t.Value)) // same place as before
				written[k] = true
			}
			continue
		}
		out = append(out, line)
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	for _, p := range prefs {
		if t := target[p.key]; t.Set && !written[p.key] {
			out = append(out, fmt.Sprintf("user_pref(%q, %s);", p.key, t.Value))
		}
	}
	return os.WriteFile(path, []byte(strings.Join(out, nl)+nl), 0o644)
}

func findAddonByGUID(g string) (Addon, bool) {
	for _, a := range addons {
		if strings.EqualFold(a.GUID, g) {
			return a, true
		}
	}
	return Addon{}, false
}

// installedKnownAddons lists add-ons from our list that are present in the profile,
// and marks the ones this installer added.
type InstalledAddon struct {
	Addon
	Ours bool `json:"ours"`
}

func installedKnownAddons(profile string) []InstalledAddon {
	st, _ := loadState(profile)
	ours := map[string]bool{}
	if st != nil {
		for _, g := range st.Addons {
			ours[strings.ToLower(g)] = true
		}
	}
	var out []InstalledAddon
	for _, a := range addons {
		if _, err := os.Stat(filepath.Join(profile, "extensions", a.GUID+".xpi")); err == nil {
			out = append(out, InstalledAddon{a, ours[strings.ToLower(a.GUID)]})
		}
	}
	return out
}

// ---------- add-ons ----------

func findAddon(slug string) (Addon, bool) {
	for _, a := range addons {
		if a.Slug == slug {
			return a, true
		}
	}
	return Addon{}, false
}

func installAddons(profile string, slugs []string, report Progress) {
	dir := filepath.Join(profile, "extensions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		report("addons", "fail", "Could not create the extensions folder")
		return
	}
	var policyFiles []string
	var policyAddons []Addon
	for _, s := range slugs {
		a, ok := findAddon(s)
		if !ok || (a.Manual && !policySupported()) {
			continue
		}
		id := "addon:" + a.Slug
		dst := filepath.Join(dir, a.GUID+".xpi")
		if _, err := os.Stat(dst); err == nil {
			if a.Manual && addonAppDisabled(profile, a.GUID) {
				// A copy Thunderbird switched off: replace it with a proper install.
				_ = os.Remove(dst)
			} else {
				report(id, "skip", a.Name+" is already installed")
				continue
			}
		}
		report(id, "run", "Downloading "+a.Name)
		if a.Manual {
			if err := os.MkdirAll(addonCacheDir(), 0o755); err != nil {
				report(id, "fail", a.Name+": "+err.Error())
				continue
			}
			f := filepath.Join(addonCacheDir(), a.Slug+".xpi")
			if err := downloadAddon(a, f); err != nil {
				report(id, "fail", a.Name+": "+err.Error())
				continue
			}
			policyFiles = append(policyFiles, f)
			policyAddons = append(policyAddons, a)
			report(id, "run", a.Name+" downloaded")
			continue
		}
		if err := downloadAddon(a, dst); err != nil {
			report(id, "fail", a.Name+": "+err.Error())
			continue
		}
		rememberAddon(profile, a.GUID)
		report(id, "ok", a.Name+" added")
	}
	if len(policyFiles) > 0 {
		report("policy", "run", "Asking Windows for permission to add full-access add-ons")
		removePrefs(profile, policyRunOncePref)
		if err := queuePolicyInstall(policyFiles); err != nil {
			report("policy", "fail", "Full-access add-ons were not added: "+err.Error())
			for _, a := range policyAddons {
				report("addon:"+a.Slug, "fail", a.Name+" was not added")
			}
		} else {
			report("policy", "ok", "Permission given")
			for _, a := range policyAddons {
				rememberAddon(profile, a.GUID)
				report("addon:"+a.Slug, "ok", a.Name+" added")
			}
		}
	}
	var guids []string
	for _, s := range slugs {
		if a, ok := findAddon(s); ok && !a.Manual {
			guids = append(guids, a.GUID)
		}
	}
	enableSideloaded(profile, guids)
	applySiteCompat(profile)
	askForCompatCheck(profile)
}

// applySiteCompat gives Thunderbird the version range the add-ons site has confirmed,
// the same thing Thunderbird does itself when it checks for add-on updates.
func applySiteCompat(profile string) {
	path := filepath.Join(profile, "extensions.json")
	b, err := os.ReadFile(path)
	if err != nil || len(compatSeen) == 0 {
		return
	}
	var db map[string]interface{}
	if json.Unmarshal(b, &db) != nil {
		return
	}
	app := tbMajor(readPref(profile, "extensions.lastAppVersion"))
	list, _ := db["addons"].([]interface{})
	changed := false
	for _, x := range list {
		a, _ := x.(map[string]interface{})
		id, _ := a["id"].(string)
		c, ok := compatSeen[strings.ToLower(id)]
		if !ok || a["version"] != c.Version {
			continue
		}
		a["targetApplications"] = []interface{}{map[string]interface{}{"id": "toolkit@mozilla.org", "minVersion": c.Min, "maxVersion": c.Max}}
		if app > 0 && tbMajor(c.Min) <= app && (c.Max == "*" || tbMajor(c.Max) >= app) {
			a["appDisabled"] = false
		}
		changed = true
	}
	if !changed {
		return
	}
	if out, err := json.Marshal(db); err == nil {
		_ = os.WriteFile(path, out, 0o644)
	}
}

func tbMajor(v string) int {
	v = strings.Trim(v, "\" ")
	n := 0
	for _, r := range v {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func readPref(profile, key string) string {
	b, err := os.ReadFile(filepath.Join(profile, "prefs.js"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		if prefKey(t) == key {
			return prefValue(t)
		}
	}
	return ""
}

// removePrefs deletes the given keys from prefs.js (Thunderbird must be closed).
func removePrefs(profile string, keys ...string) {
	path := filepath.Join(profile, "prefs.js")
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	nl := "\n"
	if strings.Contains(string(b), "\r\n") {
		nl = "\r\n"
	}
	drop := map[string]bool{}
	for _, k := range keys {
		drop[k] = true
	}
	var out []string
	hit := false
	for _, line := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		if drop[prefKey(strings.TrimSpace(line))] {
			hit = true
			continue
		}
		out = append(out, line)
	}
	if hit {
		_ = os.WriteFile(path, []byte(strings.Join(out, nl)), 0o644)
	}
}

// Thunderbird runs the add-on install policy only when its list changes. Forgetting the
// last run makes it install again, e.g. after an uninstall and a fresh install.
const policyRunOncePref = "browser.policies.runOncePerModification.extensionsInstall"

// askForCompatCheck makes Thunderbird run its own add-on update check shortly after it
// opens, so newly added add-ons pick up the compatibility the add-ons site confirmed.
func askForCompatCheck(profile string) {
	const key = "app.update.lastUpdateTime.addon-background-update-timer"
	path := filepath.Join(profile, "prefs.js")
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	nl := "\n"
	if strings.Contains(string(b), "\r\n") {
		nl = "\r\n"
	}
	var out []string
	for _, line := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		if prefKey(strings.TrimSpace(line)) == key {
			continue
		}
		out = append(out, line)
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	out = append(out, fmt.Sprintf("user_pref(%q, 1);", key))
	_ = os.WriteFile(path, []byte(strings.Join(out, nl)+nl), 0o644)
}

func rememberAddon(profile, guid string) {
	if st, err := loadState(profile); err == nil {
		for _, g := range st.Addons {
			if strings.EqualFold(g, guid) {
				return
			}
		}
		st.Addons = append(st.Addons, guid)
		_ = saveState(profile, st)
	}
}

// addonAppDisabled reports whether Thunderbird has switched an add-on off on its own.
func addonAppDisabled(profile, guid string) bool {
	b, err := os.ReadFile(filepath.Join(profile, "extensions.json"))
	if err != nil {
		return false
	}
	var db struct {
		Addons []struct {
			ID          string `json:"id"`
			AppDisabled bool   `json:"appDisabled"`
		} `json:"addons"`
	}
	if json.Unmarshal(b, &db) != nil {
		return false
	}
	for _, a := range db.Addons {
		if strings.EqualFold(a.ID, guid) {
			return a.AppDisabled
		}
	}
	return false
}

// enableSideloaded marks the given add-ons as wanted in Thunderbird's extensions.json.
// Only entries that Thunderbird itself disabled after finding them in the profile folder
// are touched. Thunderbird must be closed.
func enableSideloaded(profile string, guids []string) {
	path := filepath.Join(profile, "extensions.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var db map[string]interface{}
	if json.Unmarshal(b, &db) != nil {
		return
	}
	list, _ := db["addons"].([]interface{})
	want := map[string]bool{}
	for _, g := range guids {
		want[strings.ToLower(g)] = true
	}
	changed := false
	for _, x := range list {
		a, _ := x.(map[string]interface{})
		id, _ := a["id"].(string)
		if !want[strings.ToLower(id)] || a["location"] != "app-profile" {
			continue
		}
		if seen, _ := a["seen"].(bool); seen {
			continue // the user has already made a choice for this one
		}
		a["userDisabled"] = false
		a["seen"] = true
		changed = true
	}
	if !changed {
		return
	}
	out, err := json.Marshal(db)
	if err != nil {
		return
	}
	_ = os.WriteFile(path, out, 0o644)
}

type apiAddon struct {
	GUID           string `json:"guid"`
	CurrentVersion struct {
		Version       string `json:"version"`
		Compatibility map[string]struct {
			Min string `json:"min"`
			Max string `json:"max"`
		} `json:"compatibility"`
		Files []struct {
			URL  string `json:"url"`
			Hash string `json:"hash"`
		} `json:"files"`
	} `json:"current_version"`
}

var client = &http.Client{Timeout: 60 * time.Second}

func get(url string) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent())
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("no internet connection, or the add-ons site is unreachable")
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		return nil, fmt.Errorf("the add-ons site answered %s", resp.Status)
	}
	return resp, nil
}

// addonCompat is the Thunderbird range the add-ons site has confirmed for a version.
type addonCompat struct{ Version, Min, Max string }

var compatSeen = map[string]addonCompat{}

func downloadAddon(a Addon, dst string) error {
	resp, err := get(apiBase + a.Slug + "/")
	if err != nil {
		return err
	}
	var info apiAddon
	err = json.NewDecoder(resp.Body).Decode(&info)
	resp.Body.Close()
	if err != nil {
		return errors.New("unexpected answer from the add-ons site")
	}
	if !strings.EqualFold(info.GUID, a.GUID) {
		return errors.New("add-on id did not match, skipped for safety")
	}
	if len(info.CurrentVersion.Files) == 0 {
		return errors.New("no download available")
	}
	if c, ok := info.CurrentVersion.Compatibility["thunderbird"]; ok {
		compatSeen[strings.ToLower(a.GUID)] = addonCompat{info.CurrentVersion.Version, c.Min, c.Max}
	}
	f := info.CurrentVersion.Files[0]
	if !strings.HasPrefix(f.URL, "https://addons.thunderbird.net/") {
		return errors.New("download is not from addons.thunderbird.net, skipped")
	}
	algo, want, ok := strings.Cut(f.Hash, ":")
	if !ok || algo != "sha256" {
		return errors.New("no checksum to verify, skipped")
	}
	resp, err = get(f.URL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 100<<20))
	if err != nil {
		return errors.New("download was interrupted")
	}
	sum := sha256.Sum256(data)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), want) {
		return errors.New("checksum did not match, file not saved")
	}
	tmp := dst + ".part"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
