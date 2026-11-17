//go:build windows

package main

// Experiment add-ons (the ones with "full access") are switched off by Thunderbird when a
// program drops them into the profile folder. Thunderbird does accept them through its
// official enterprise policy "Extensions > Install". The installer uses that, only for the
// add-ons the user ticked:
//
//   HKEY_USERS\<you>\Software\Policies\Mozilla\Thunderbird\Extensions\Install\<n> = file:///...xpi
//
// Windows protects the Policies key, so this one step asks for admin permission once.
// Thunderbird installs each add-on once at its next start. Uninstall removes the values.

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const policyInstallKey = `Software\Policies\Mozilla\Thunderbird\Extensions\Install`

func policySupported() bool { return true }

func addonCacheDir() string { return filepath.Join(appDataDir(), "addons") }

func fileURL(p string) string {
	u := url.URL{Scheme: "file", Path: "/" + filepath.ToSlash(p)}
	return u.String()
}

func currentUserSID() (string, error) {
	tok := windows.GetCurrentProcessToken()
	u, err := tok.GetTokenUser()
	if err != nil {
		return "", err
	}
	return u.User.Sid.String(), nil
}

// queuePolicyInstall asks for admin once and registers the files for Thunderbird to install.
func queuePolicyInstall(files []string) error {
	if len(files) == 0 {
		return nil
	}
	sid, err := currentUserSID()
	if err != nil {
		return err
	}
	args := []string{"--policy-add", sid}
	for _, f := range files {
		args = append(args, fileURL(f))
	}
	return runElevated(args)
}

// removePolicyInstalls removes every value this program added (they all point into its folder).
func removePolicyInstalls() error {
	if !havePolicyValues() {
		return nil
	}
	sid, err := currentUserSID()
	if err != nil {
		return err
	}
	return runElevated([]string{"--policy-remove", sid, fileURL(addonCacheDir())})
}

// havePolicyValues checks (read-only, no admin) whether any of our values exist.
func havePolicyValues() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, policyInstallKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	names, _ := k.ReadValueNames(0)
	prefix := strings.ToLower(fileURL(addonCacheDir()))
	for _, n := range names {
		if v, _, err := k.GetStringValue(n); err == nil && strings.HasPrefix(strings.ToLower(v), prefix) {
			return true
		}
	}
	return false
}

// ---------- elevated side ----------

// runPolicyCommand runs inside the elevated copy: --policy-add SID url... / --policy-remove SID prefix
func runPolicyCommand(args []string) int {
	if len(args) < 2 || !strings.HasPrefix(args[1], "S-1-") {
		return 2
	}
	cmd, sid, rest := args[0], args[1], args[2:]
	base := sid + `\` + policyInstallKey
	switch cmd {
	case "--policy-add":
		k, _, err := registry.CreateKey(registry.USERS, base, registry.ALL_ACCESS)
		if err != nil {
			return 3
		}
		defer k.Close()
		names, _ := k.ReadValueNames(0)
		have := map[string]bool{}
		next := 1
		for _, n := range names {
			if v, _, err := k.GetStringValue(n); err == nil {
				have[strings.ToLower(v)] = true
			}
			if i, err := strconv.Atoi(n); err == nil && i >= next {
				next = i + 1
			}
		}
		for _, u := range rest {
			if !strings.HasPrefix(u, "file:///") || !strings.HasSuffix(strings.ToLower(u), ".xpi") || have[strings.ToLower(u)] {
				continue
			}
			if err := k.SetStringValue(strconv.Itoa(next), u); err != nil {
				return 4
			}
			next++
		}
		return 0
	case "--policy-remove":
		if len(rest) != 1 || !strings.HasPrefix(rest[0], "file:///") {
			return 2
		}
		prefix := strings.ToLower(rest[0])
		k, err := registry.OpenKey(registry.USERS, base, registry.ALL_ACCESS)
		if err != nil {
			return 0
		}
		names, _ := k.ReadValueNames(0)
		for _, n := range names {
			if v, _, err := k.GetStringValue(n); err == nil && strings.HasPrefix(strings.ToLower(v), prefix) {
				_ = k.DeleteValue(n)
			}
		}
		left, _ := k.ReadValueNames(0)
		k.Close()
		if len(left) == 0 {
			// Remove the keys we created, but only while they are empty.
			for _, p := range []string{policyInstallKey, `Software\Policies\Mozilla\Thunderbird\Extensions`, `Software\Policies\Mozilla\Thunderbird`, `Software\Policies\Mozilla`} {
				if isEmptyKey(sid + `\` + p) {
					_ = registry.DeleteKey(registry.USERS, sid+`\`+p)
				} else {
					break
				}
			}
		}
		return 0
	}
	return 2
}

func isEmptyKey(path string) bool {
	k, err := registry.OpenKey(registry.USERS, path, registry.QUERY_VALUE|registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return false
	}
	defer k.Close()
	st, err := k.Stat()
	return err == nil && st.SubKeyCount == 0 && st.ValueCount == 0
}

// ---------- ShellExecuteEx "runas" ----------

type shellExecuteInfo struct {
	cbSize         uint32
	fMask          uint32
	hwnd           uintptr
	lpVerb         *uint16
	lpFile         *uint16
	lpParameters   *uint16
	lpDirectory    *uint16
	nShow          int32
	hInstApp       uintptr
	lpIDList       uintptr
	lpClass        *uint16
	hkeyClass      uintptr
	dwHotKey       uint32
	hIconOrMonitor uintptr
	hProcess       windows.Handle
}

func runElevated(args []string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	var q []string
	for _, a := range args {
		q = append(q, syscall.EscapeArg(a))
	}
	verb, _ := windows.UTF16PtrFromString("runas")
	file, _ := windows.UTF16PtrFromString(self)
	params, _ := windows.UTF16PtrFromString(strings.Join(q, " "))
	info := shellExecuteInfo{fMask: 0x00000040 /* SEE_MASK_NOCLOSEPROCESS */, lpVerb: verb, lpFile: file, lpParameters: params, nShow: 0}
	info.cbSize = uint32(unsafe.Sizeof(info))
	proc := windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteExW")
	r, _, e := proc.Call(uintptr(unsafe.Pointer(&info)))
	if r == 0 {
		if e == windows.ERROR_CANCELLED {
			return fmt.Errorf("admin permission was not given")
		}
		return fmt.Errorf("could not ask for admin permission: %v", e)
	}
	defer windows.CloseHandle(info.hProcess)
	windows.WaitForSingleObject(info.hProcess, windows.INFINITE)
	var code uint32
	_ = windows.GetExitCodeProcess(info.hProcess, &code)
	if code != 0 {
		return fmt.Errorf("could not register the add-ons (code %d)", code)
	}
	return nil
}
