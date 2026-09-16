package schedule

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestScheduleInstallRemove(t *testing.T) {
	for _, platform := range []string{"darwin", "linux"} {
		t.Run(platform, func(t *testing.T) {
			home := t.TempDir()
			exe := filepath.Join(home, "my bin", "updater")
			state := filepath.Join(home, "my state")
			var calls []string
			run := func(_ context.Context, name string, args ...string) error {
				calls = append(calls, name+" "+strings.Join(args, " "))
				return nil
			}
			for i := 0; i < 2; i++ {
				if err := install(context.Background(), platform, home, exe, state, filepath.Join(home, "config.json"), run); err != nil {
					t.Fatal(err)
				}
			}
			plans, err := files(platform, home, exe, state, filepath.Join(home, "config.json"))
			if err != nil {
				t.Fatal(err)
			}
			for path, content := range plans {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != content {
					t.Fatalf("saved file %s: %v", path, err)
				}
				info, _ := os.Stat(path)
				if info.Mode().Perm() != 0600 {
					t.Fatal("schedule not private")
				}
			}
			if err := remove(context.Background(), platform, home, exe, state, filepath.Join(home, "config.json"), run); err != nil {
				t.Fatal(err)
			}
			for path := range plans {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("not removed: %s", path)
				}
			}
			if len(calls) == 0 {
				t.Fatal("no scheduler commands")
			}
			if platform == "linux" && (!strings.Contains(strings.Join(calls, "\n"), "systemctl --user enable --now t3-update-preview.timer") || !strings.Contains(strings.Join(calls, "\n"), "systemctl --user disable --now t3-update-preview.timer")) {
				t.Fatal(calls)
			}
		})
	}
}

func TestScheduleEscaping(t *testing.T) {
	home := t.TempDir()
	exe := filepath.Join(home, "app space $HOME %u &\" <tag>")
	state := filepath.Join(home, "state $USER %h")
	for _, platform := range []string{"darwin", "linux"} {
		plans, err := files(platform, home, exe, state, filepath.Join(home, "config.json"))
		if err != nil {
			t.Fatal(err)
		}
		for path, content := range plans {
			if platform == "darwin" {
				dec := xml.NewDecoder(strings.NewReader(content))
				var values []string
				for {
					token, err := dec.Token()
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
					if start, ok := token.(xml.StartElement); ok && start.Name.Local == "string" {
						var value string
						if err := dec.DecodeElement(&value, &start); err != nil {
							t.Fatal(err)
						}
						values = append(values, value)
					}
				}
				if !reflect.DeepEqual(values, []string{label, exe, "--scheduled-check", "--state-dir", state, "--config", filepath.Join(home, "config.json"), os.Getenv("PATH") + ":/usr/bin:/bin:/usr/sbin:/sbin"}) {
					t.Fatal(values)
				}
				if runtime.GOOS == "darwin" {
					fixture := filepath.Join(t.TempDir(), "schedule.plist")
					if err := os.WriteFile(fixture, []byte(content), 0600); err != nil {
						t.Fatal(err)
					}
					if output, err := exec.Command("plutil", "-lint", fixture).CombinedOutput(); err != nil {
						t.Fatalf("%v: %s", err, output)
					}
				}
			} else if strings.HasSuffix(path, ".service") {
				if !strings.Contains(content, "ExecStart=:") || !strings.Contains(content, "$HOME %%u") || !strings.Contains(content, "$USER %%h") || !strings.Contains(content, "app space") || !strings.Contains(content, "\\\"") {
					t.Fatal(content)
				}
			} else if !strings.Contains(content, "Persistent=true") || !strings.Contains(content, "OnUnitInactiveSec=5min") || !strings.Contains(content, "OnCalendar=hourly") {
				t.Fatal(content)
			}
		}
	}
}

func TestNotificationInputIsArgument(t *testing.T) {
	version := "v1\"; do shell script \"touch /tmp/should-not-exist\"; $HOME %u"
	for _, platform := range []string{"darwin", "linux"} {
		var gotName string
		var gotArgs []string
		err := Notify(context.Background(), platform, version, func(_ context.Context, name string, args ...string) error { gotName = name; gotArgs = args; return nil })
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(gotArgs[len(gotArgs)-1], version) {
			t.Fatal(gotArgs)
		}
		if platform == "darwin" && (gotName != "osascript" || strings.Contains(gotArgs[1], version) || gotArgs[2] != "--") {
			t.Fatal(gotArgs)
		}
		if platform == "linux" && gotName != "notify-send" {
			t.Fatal(gotName)
		}
	}
}

func TestRefuseUnownedFilesAndSymlinks(t *testing.T) {
	for _, kind := range []string{"unrelated", "file symlink", "directory symlink"} {
		t.Run(kind, func(t *testing.T) {
			home := t.TempDir()
			dir := filepath.Join(home, ".config", "systemd", "user")
			path := filepath.Join(dir, unit+".service")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "unrelated":
				if err := os.WriteFile(path, []byte("other app"), 0600); err != nil {
					t.Fatal(err)
				}
			case "file symlink":
				if err := os.Symlink(filepath.Join(t.TempDir(), "other"), path); err != nil {
					t.Fatal(err)
				}
			case "directory symlink":
				if err := os.Remove(dir); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), dir); err != nil {
					t.Fatal(err)
				}
			}
			run := func(context.Context, string, ...string) error { t.Fatal("scheduler called"); return nil }
			if err := install(context.Background(), "linux", home, "/bin/updater", filepath.Join(home, "state"), filepath.Join(home, "config.json"), run); err == nil {
				t.Fatal("unsafe install accepted")
			}
			if err := remove(context.Background(), "linux", home, "/bin/updater", filepath.Join(home, "state"), filepath.Join(home, "config.json"), run); err == nil {
				t.Fatal("unsafe removal accepted")
			}
		})
	}
}

func TestMissingSchedulerReturnsError(t *testing.T) {
	home := t.TempDir()
	err := install(context.Background(), "linux", home, "/bin/updater", filepath.Join(home, "state"), filepath.Join(home, "config.json"), func(context.Context, string, ...string) error { return errors.New("systemd unavailable") })
	if err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatal(err)
	}
}

func TestScheduleCapturesOnlySafeSetupPATH(t *testing.T) {
	searchPath := "/chosen tools/$literal/%u/quoted\"&dir:/usr/local/bin"
	t.Setenv("PATH", searchPath)
	t.Setenv("T3_TEST_PRIVATE_VALUE", "must-not-be-written")
	for _, platform := range []string{"darwin", "linux"} {
		home := t.TempDir()
		run := func(context.Context, string, ...string) error { return nil }
		if err := install(context.Background(), platform, home, "/bin/updater", filepath.Join(home, "state"), filepath.Join(home, "config.json"), run); err != nil {
			t.Fatal(err)
		}
		plans, err := files(platform, home, "/bin/updater", filepath.Join(home, "state"), filepath.Join(home, "config.json"))
		if err != nil {
			t.Fatal(err)
		}
		for path := range plans {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			content := string(data)
			if strings.Contains(content, "must-not-be-written") || strings.Contains(content, "T3_TEST_PRIVATE_VALUE") {
				t.Fatal("unrelated environment saved")
			}
			if platform == "darwin" {
				if !strings.Contains(content, "<key>EnvironmentVariables</key><dict><key>PATH</key>") || !strings.Contains(content, "/chosen tools/$literal/%u/quoted&#34;&amp;dir:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin") {
					t.Fatal("setup PATH missing from plist")
				}
			} else if strings.HasSuffix(path, ".service") && !strings.Contains(content, "Environment=\"PATH=/chosen tools/$literal/%%u/quoted\\\"&dir:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin\"") {
				t.Fatal("setup PATH missing from service")
			}
		}
		t.Setenv("PATH", "")
		if err := remove(context.Background(), platform, home, "/bin/updater", filepath.Join(home, "state"), filepath.Join(home, "config.json"), run); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", searchPath)
	}
}

func TestInstallRejectsUnsafePATHBeforeWriting(t *testing.T) {
	for _, value := range []string{"", ".:/usr/bin", "/usr/bin:", "/usr/bin::/bin", "bin:/usr/bin", "/usr/bin\nInjected=yes"} {
		t.Setenv("PATH", value)
		home := t.TempDir()
		if err := install(context.Background(), "linux", home, "/bin/updater", filepath.Join(home, "state"), filepath.Join(home, "config.json"), func(context.Context, string, ...string) error { t.Fatal("runner called"); return nil }); err == nil {
			t.Fatalf("accepted unsafe PATH %q", value)
		}
		entries, err := os.ReadDir(home)
		if err != nil || len(entries) != 0 {
			t.Fatalf("installation wrote with unsafe PATH: %v", err)
		}
	}
}
