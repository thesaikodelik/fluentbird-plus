//go:build windows

package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
)

func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}

const taskName = "Fluentbird+ Update"

// registerTask creates (or replaces) a per-user daily task. No admin rights needed.
func registerTask(exe string) error {
	esc := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;").Replace
	start := time.Now().Add(10 * time.Minute).Format("2006-01-02T15:04:05")
	xml := `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo><Description>Keeps the Fluentbird+ theme for Thunderbird up to date. Remove it by uninstalling Fluentbird+.</Description></RegistrationInfo>
  <Triggers>
    <CalendarTrigger>
      <StartBoundary>` + start + `</StartBoundary>
      <ScheduleByDay><DaysInterval>1</DaysInterval></ScheduleByDay>
    </CalendarTrigger>
  </Triggers>
  <Principals><Principal id="Author"><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals>
  <Settings>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>true</RunOnlyIfNetworkAvailable>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <ExecutionTimeLimit>PT10M</ExecutionTimeLimit>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
  </Settings>
  <Actions Context="Author"><Exec><Command>` + esc(exe) + `</Command><Arguments>--auto-update</Arguments></Exec></Actions>
</Task>`
	// schtasks wants UTF-16 with a byte order mark.
	u := utf16.Encode([]rune(xml))
	buf := make([]byte, 2+len(u)*2)
	buf[0], buf[1] = 0xFF, 0xFE
	for i, v := range u {
		binary.LittleEndian.PutUint16(buf[2+i*2:], v)
	}
	f := filepath.Join(os.TempDir(), "fluentbird-plus-task.xml")
	if err := os.WriteFile(f, buf, 0o644); err != nil {
		return err
	}
	defer os.Remove(f)
	cmd := exec.Command("schtasks", "/Create", "/F", "/TN", taskName, "/XML", f)
	hideWindow(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("could not schedule updates: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func unregisterTask() {
	cmd := exec.Command("schtasks", "/Delete", "/F", "/TN", taskName)
	hideWindow(cmd)
	_ = cmd.Run()
}
