package schedule

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unicode"
)

type Runner func(context.Context, string, ...string) error

const unit = "t3-update-preview"
const serviceHeader = "[Unit]\nDescription=T3 preview update checks\n"
const timerHeader = "[Unit]\nDescription=T3 preview update check timer\n"

func Install(ctx context.Context, executable, stateDir, configPath string, run Runner) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	return install(ctx, runtime.GOOS, home, executable, stateDir, configPath, run)
}

func Remove(ctx context.Context, executable, stateDir, configPath string, run Runner) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	return remove(ctx, runtime.GOOS, home, executable, stateDir, configPath, run)
}

func files(platform, home, executable, stateDir, configPath string) (map[string]string, error) {
	for _, path := range []string{home, executable, stateDir, configPath} {
		if !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n") {
			return nil, errors.New("scheduler paths must be absolute and contain no line breaks")
		}
	}
	searchPath := os.Getenv("PATH") + ":/usr/bin:/bin:/usr/sbin:/sbin"
	switch platform {
	case "darwin":
		var exe, state, config, envPath bytes.Buffer
		xml.EscapeText(&exe, []byte(executable))
		xml.EscapeText(&state, []byte(stateDir))
		xml.EscapeText(&config, []byte(configPath))
		xml.EscapeText(&envPath, []byte(searchPath))
		body := "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<plist version=\"1.0\"><dict>\n" +
			"<key>Label</key><string>" + label + "</string>\n" +
			"<key>ProgramArguments</key><array><string>" + exe.String() + "</string><string>--scheduled-check</string><string>--state-dir</string><string>" + state.String() + "</string><string>--config</string><string>" + config.String() + "</string></array>\n" +
			"<key>EnvironmentVariables</key><dict><key>PATH</key><string>" + envPath.String() + "</string></dict>\n" +
			"<key>RunAtLoad</key><true/>\n<key>StartCalendarInterval</key><dict><key>Minute</key><integer>0</integer></dict>\n<key>StartInterval</key><integer>300</integer>\n</dict></plist>\n"
		return map[string]string{filepath.Join(home, "Library", "LaunchAgents", label+".plist"): body}, nil
	case "linux":
		dir := filepath.Join(home, ".config", "systemd", "user")
		return map[string]string{
			filepath.Join(dir, unit+".service"): serviceHeader + "\n[Service]\nType=oneshot\nEnvironment=" + systemdQuote("PATH="+searchPath) + "\nExecStart=:" + systemdQuote(executable) + " --scheduled-check --state-dir " + systemdQuote(stateDir) + " --config " + systemdQuote(configPath) + "\n",
			filepath.Join(dir, unit+".timer"):   timerHeader + "\n[Timer]\nOnCalendar=hourly\nPersistent=true\nOnStartupSec=30sec\nOnUnitInactiveSec=5min\n\n[Install]\nWantedBy=timers.target\n",
		}, nil
	default:
		return nil, fmt.Errorf("user scheduling is unsupported on %s", platform)
	}
}

func systemdQuote(value string) string {
	return strconv.Quote(strings.ReplaceAll(value, "%", "%%"))
}

func install(ctx context.Context, platform, home, executable, stateDir, configPath string, run Runner) error {
	for _, directory := range strings.Split(os.Getenv("PATH"), string(os.PathListSeparator)) {
		if !filepath.IsAbs(directory) || strings.IndexFunc(directory, unicode.IsControl) >= 0 {
			return errors.New("schedule setup requires PATH with only absolute directories and no empty entries or control characters")
		}
	}
	plans, err := files(platform, home, executable, stateDir, configPath)
	if err != nil {
		return err
	}
	for path := range plans {
		if err := checkPath(home, path); err != nil {
			return err
		}
		if _, err := owned(path); err != nil {
			return err
		}
	}
	for path, content := range plans {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		if err := write(path, content); err != nil {
			return err
		}
	}
	if platform == "linux" {
		if err := run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
			return fmt.Errorf("user systemd is required; schedule files saved but not enabled: %w", err)
		}
		return run(ctx, "systemctl", "--user", "enable", "--now", unit+".timer")
	}
	target := "gui/" + strconv.Itoa(os.Getuid())
	if run(ctx, "launchctl", "print", target+"/"+label) == nil {
		if err := run(ctx, "launchctl", "bootout", target+"/"+label); err != nil {
			return err
		}
	}
	return run(ctx, "launchctl", "bootstrap", target, filepath.Join(home, "Library", "LaunchAgents", label+".plist"))
}

func remove(ctx context.Context, platform, home, executable, stateDir, configPath string, run Runner) error {
	plans, err := files(platform, home, executable, stateDir, configPath)
	if err != nil {
		return err
	}
	var paths []string
	for path := range plans {
		if err := checkPath(home, path); err != nil {
			return err
		}
		exists, err := owned(path)
		if err != nil {
			return err
		}
		if exists {
			paths = append(paths, path)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	if platform == "linux" {
		if err := run(ctx, "systemctl", "--user", "disable", "--now", unit+".timer"); err != nil {
			return err
		}
	} else {
		target := "gui/" + strconv.Itoa(os.Getuid()) + "/" + label
		if run(ctx, "launchctl", "print", target) == nil {
			if err := run(ctx, "launchctl", "bootout", target); err != nil {
				return err
			}
		}
	}
	for _, path := range paths {
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	if platform == "linux" {
		return run(ctx, "systemctl", "--user", "daemon-reload")
	}
	return nil
}

func checkPath(home, path string) error {
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		info, err := os.Lstat(dir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && (!info.IsDir() || info.Mode().Perm()&0022 != 0 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid())) {
			return fmt.Errorf("scheduler directory must be owned by the current user and not writable by others: %s", dir)
		}
		if dir == home {
			return nil
		}
		if dir == filepath.Dir(dir) {
			return errors.New("scheduler path is outside the user home")
		}
	}
}

func owned(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		return false, fmt.Errorf("refusing to replace or remove unowned schedule file: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	body := string(data)
	valid := false
	switch filepath.Base(path) {
	case unit + ".service":
		valid = strings.HasPrefix(body, serviceHeader) && strings.Contains(body, " --scheduled-check --state-dir ")
	case unit + ".timer":
		valid = strings.HasPrefix(body, timerHeader)
	case label + ".plist":
		valid = strings.Contains(body, "<key>Label</key><string>"+label+"</string>") && strings.Contains(body, "<string>--scheduled-check</string>")
	}
	if !valid {
		return false, fmt.Errorf("refusing to replace or remove an unrelated schedule file: %s", path)
	}
	return true, nil
}

func write(path, content string) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".t3-update-preview-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, err = file.WriteString(content)
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func Notify(ctx context.Context, platform, version string, run Runner) error {
	message := "T3 preview " + version + " is available. Run t3-update-preview to update."
	switch platform {
	case "darwin":
		return run(ctx, "osascript", "-e", "on run argv\ndisplay notification (item 1 of argv) with title \"T3 preview update\"\nend run", "--", message)
	case "linux":
		return run(ctx, "notify-send", "--app-name=t3-update-preview", "--hint=string:x-canonical-private-synchronous:t3-update-preview", "--", "T3 preview update", message)
	default:
		return fmt.Errorf("desktop notifications are unsupported on %s", platform)
	}
}

const label = "io.github.bil0000.t3-update-preview"
