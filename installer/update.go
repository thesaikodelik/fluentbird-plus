package main

// Automatic updates
//
// When the user ticks "Keep Fluentbird+ up to date", the installer:
//   1. copies itself to %LOCALAPPDATA%\FluentbirdPlus\FluentbirdPlus.exe
//   2. remembers which profiles it manages in config.json next to it
//   3. adds a per-user Windows scheduled task that runs "FluentbirdPlus.exe --auto-update" once a day
//
// --auto-update asks GitHub for the latest release of updateRepo. If it is newer, it downloads
// the new installer, checks its SHA-256 against the checksum file published with the release,
// swaps it in, and runs it with --apply-update, which refreshes the theme files in each
// managed profile. Your backups and settings are kept. Add-ons are updated by Thunderbird itself.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Overridable for tests.
var (
	githubAPI = "https://api.github.com"
	githubWeb = "https://github.com"
)

const (
	exeName   = "FluentbirdPlus.exe"
	assetExe  = "FluentbirdPlus-Setup.exe"
	assetHash = "FluentbirdPlus-Setup.exe.sha256"
)

type updateConfig struct {
	Repo      string   `json:"repo"`
	Profiles  []string `json:"profiles"`
	Version   string   `json:"version"`
	LastCheck string   `json:"lastCheck,omitempty"`
}

func autoUpdateSupported() bool { return updateRepo != "" && runtime.GOOS == "windows" }

func appDataDir() string {
	if base := os.Getenv("LOCALAPPDATA"); base != "" {
		return filepath.Join(base, "FluentbirdPlus")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "fluentbird-plus")
}

func configPath() string { return filepath.Join(appDataDir(), "config.json") }

func loadConfig() (*updateConfig, error) {
	b, err := os.ReadFile(configPath())
	if err != nil {
		return nil, err
	}
	var c updateConfig
	return &c, json.Unmarshal(b, &c)
}

func saveConfig(c *updateConfig) error {
	if err := os.MkdirAll(appDataDir(), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(configPath(), b, 0o644)
}

func autoUpdateOn(profile string) bool {
	c, err := loadConfig()
	if err != nil {
		return false
	}
	for _, p := range c.Profiles {
		if strings.EqualFold(p, profile) {
			return true
		}
	}
	return false
}

// enableAutoUpdate copies this program to the app folder and schedules the daily check.
func enableAutoUpdate(profile string) error {
	if !autoUpdateSupported() {
		return nil
	}
	dir := appDataDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	target := filepath.Join(dir, exeName)
	if !samePath(self, target) {
		if err := copyFile(self, target); err != nil {
			return fmt.Errorf("could not copy the updater: %w", err)
		}
	}
	c, err := loadConfig()
	if err != nil {
		c = &updateConfig{}
	}
	c.Repo, c.Version = updateRepo, appVersion
	if !autoUpdateOn(profile) {
		c.Profiles = append(c.Profiles, profile)
	}
	if err := saveConfig(c); err != nil {
		return err
	}
	return registerTask(target)
}

// disableAutoUpdate stops managing a profile; with none left, it removes the task and app folder.
func disableAutoUpdate(profile string) {
	c, err := loadConfig()
	if err != nil {
		return
	}
	var keep []string
	for _, p := range c.Profiles {
		if !strings.EqualFold(p, profile) {
			keep = append(keep, p)
		}
	}
	c.Profiles = keep
	if len(keep) > 0 {
		_ = saveConfig(c)
		return
	}
	unregisterTask()
	dir := appDataDir()
	self, _ := os.Executable()
	for _, f := range []string{"config.json", "update.log", exeName, "FluentbirdPlus.old.exe", "FluentbirdPlus.new.exe"} {
		p := filepath.Join(dir, f)
		if samePath(self, p) {
			continue // cannot delete a running program; it is harmless and tiny next to the folder
		}
		_ = os.Remove(p)
	}
	_ = os.Remove(dir)
}

// ---------- the daily check ----------

type ghRelease struct {
	TagName string `json:"tag_name"`
	Draft   bool   `json:"draft"`
	Pre     bool   `json:"prerelease"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func logUpdate(format string, a ...interface{}) {
	_ = os.MkdirAll(appDataDir(), 0o755)
	f, err := os.OpenFile(filepath.Join(appDataDir(), "update.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s  %s\r\n", time.Now().Format("2006-01-02 15:04"), fmt.Sprintf(format, a...))
}

func runAutoUpdate() {
	c, err := loadConfig()
	if err != nil || len(c.Profiles) == 0 || c.Repo == "" {
		return
	}
	c.LastCheck = time.Now().Format(time.RFC3339)
	_ = saveConfig(c)

	rel, err := latestRelease(c.Repo)
	if err != nil {
		logUpdate("check failed: %v", err)
		return
	}
	latest := strings.TrimPrefix(rel.TagName, "v")
	if !isNewer(latest, appVersion) {
		logUpdate("up to date (v%s)", appVersion)
		return
	}
	var exeURL, hashURL string
	prefix := githubWeb + "/" + c.Repo + "/releases/download/"
	for _, a := range rel.Assets {
		if !strings.HasPrefix(a.URL, prefix) {
			continue
		}
		switch a.Name {
		case assetExe:
			exeURL = a.URL
		case assetHash:
			hashURL = a.URL
		}
	}
	if exeURL == "" || hashURL == "" {
		logUpdate("v%s has no installer or checksum, skipped", latest)
		return
	}
	data, err := fetch(exeURL, 50<<20)
	if err != nil {
		logUpdate("download failed: %v", err)
		return
	}
	sumFile, err := fetch(hashURL, 4096)
	if err != nil {
		logUpdate("checksum download failed: %v", err)
		return
	}
	want := strings.Fields(string(sumFile))
	got := sha256.Sum256(data)
	if len(want) == 0 || !strings.EqualFold(want[0], hex.EncodeToString(got[:])) {
		logUpdate("checksum mismatch for v%s, skipped", latest)
		return
	}

	dir := appDataDir()
	cur := filepath.Join(dir, exeName)
	newPath := filepath.Join(dir, "FluentbirdPlus.new.exe")
	oldPath := filepath.Join(dir, "FluentbirdPlus.old.exe")
	if err := os.WriteFile(newPath, data, 0o755); err != nil {
		logUpdate("could not save update: %v", err)
		return
	}
	_ = os.Remove(oldPath)
	if err := os.Rename(cur, oldPath); err != nil {
		logUpdate("could not replace program: %v", err)
		return
	}
	if err := os.Rename(newPath, cur); err != nil {
		_ = os.Rename(oldPath, cur)
		logUpdate("could not replace program: %v", err)
		return
	}
	logUpdate("downloaded v%s", latest)
	cmd := exec.Command(cur, "--apply-update")
	hideWindow(cmd)
	_ = cmd.Start()
}

// runApplyUpdate refreshes the theme files in every managed profile with this version's files.
func runApplyUpdate() {
	time.Sleep(2 * time.Second) // let the old process exit
	_ = os.Remove(filepath.Join(appDataDir(), "FluentbirdPlus.old.exe"))
	c, err := loadConfig()
	if err != nil {
		return
	}
	quiet := func(id, status, msg string) {}
	for _, p := range c.Profiles {
		if !validProfile(p) || !isInstalled(p) {
			logUpdate("skipped a profile that no longer has Fluentbird+")
			continue
		}
		if _, err := installTheme(p, quiet); err != nil {
			logUpdate("update failed for a profile: %v", err)
			continue
		}
	}
	c.Version = appVersion
	_ = saveConfig(c)
	if self, err := os.Executable(); err == nil {
		_ = registerTask(self) // keep the task pointing at the right program
	}
	logUpdate("updated to v%s (takes effect next time Thunderbird opens)", appVersion)
}

func latestRelease(repo string) (*ghRelease, error) {
	resp, err := get(githubAPI + "/repos/" + repo + "/releases/latest")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var r ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, errors.New("unexpected answer from GitHub")
	}
	if r.Draft || r.Pre || r.TagName == "" {
		return nil, errors.New("no published release")
	}
	return &r, nil
}

func fetch(url string, limit int64) ([]byte, error) {
	resp, err := get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// isNewer compares dotted versions like 1.10.0 and 1.9.2.
func isNewer(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(strings.TrimFunc(pa[i], func(r rune) bool { return r < '0' || r > '9' }))
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(strings.TrimFunc(pb[i], func(r rune) bool { return r < '0' || r > '9' }))
		}
		if x != y {
			return x > y
		}
	}
	return false
}

func samePath(a, b string) bool {
	ea, _ := filepath.Abs(a)
	eb, _ := filepath.Abs(b)
	return strings.EqualFold(ea, eb)
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	tmp := dst + ".part"
	if err := os.WriteFile(tmp, b, 0o755); err != nil {
		return err
	}
	_ = os.Remove(dst)
	return os.Rename(tmp, dst)
}
