package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Bil0000/t3-update-preview/internal/config"
	"github.com/Bil0000/t3-update-preview/internal/host"
	"github.com/Bil0000/t3-update-preview/internal/release"
)

type runtimeHosts struct {
	calls    []string
	fail     string
	snapshot host.Snapshot
}

func (f *runtimeHosts) Call(_ context.Context, d config.Device, r host.Request) (host.Snapshot, error) {
	f.calls = append(f.calls, d.ID+":"+r.Action)
	if f.fail == d.ID+":"+r.Action {
		return f.snapshot, fmt.Errorf("fixture unavailable")
	}
	s := f.snapshot
	s.MachineID = d.ID
	return s, nil
}
func runtimeFixture(t *testing.T) (Dependencies, *runtimeHosts) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	hosts := &runtimeHosts{snapshot: host.Snapshot{Version: "0.0.41-preview.20260916.1794", Supported: true, Busy: "idle", Status: "healthy", OS: "linux", Arch: "x64"}}
	deps := Dependencies{Paths: config.Paths{Config: filepath.Join(dir, "config.json"), State: filepath.Join(dir, "state")}, Hosts: hosts, Version: "test", Now: func() time.Time { return time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC) }, Latest: func(context.Context) (release.Release, error) {
		return release.Release{Tag: "v0.0.41-preview.20260916.1794"}, nil
	}}
	cfg := config.Config{Version: 1, Devices: []config.Device{{ID: "local", Name: "local", Kind: "local", MachineID: "local"}, {ID: "remote", Name: "remote", Kind: "ssh", Host: "server", MachineID: "remote"}}}
	if err := config.Save(deps.Paths.Config, cfg); err != nil {
		t.Fatal(err)
	}
	return deps, hosts
}
func TestReadOnlyModesAndExclusions(t *testing.T) {
	for _, mode := range []string{"--check", "--status", "--dry-run"} {
		t.Run(mode, func(t *testing.T) {
			deps, hosts := runtimeFixture(t)
			var out bytes.Buffer
			code := Run(context.Background(), []string{mode, "--exclude", "remote"}, strings.NewReader(""), &out, &out, deps)
			if code != 0 {
				t.Fatalf("exit %d: %s", code, out.String())
			}
			for _, call := range hosts.calls {
				if call != "local:probe" && call != "local:status" {
					t.Fatalf("unexpected host action %s", call)
				}
			}
		})
	}
}
func TestInvalidOptionsNeverProbe(t *testing.T) {
	for _, args := range [][]string{{"--check", "--status"}, {"--force", "--check"}, {"--wait=-1s"}, {"--import=/tmp/a"}, {"--exclude", "unknown"}, {"--config=relative"}, {"unexpected"}} {
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			deps, hosts := runtimeFixture(t)
			if Run(context.Background(), args, strings.NewReader(""), io.Discard, io.Discard, deps) == 0 {
				t.Fatal("accepted invalid arguments")
			}
			if len(hosts.calls) > 0 {
				t.Fatal(hosts.calls)
			}
		})
	}
}
func TestCheckIncludesStaleService(t *testing.T) {
	deps, hosts := runtimeFixture(t)
	hosts.snapshot.ServiceVersion = "0.0.41-preview.20260915.1772"
	var out bytes.Buffer
	if code := Run(context.Background(), []string{"--check"}, strings.NewReader(""), &out, &out, deps); code != 0 || !strings.Contains(out.String(), "is available") {
		t.Fatalf("%d %s", code, out.String())
	}
}
func TestScheduledCatchupFetchesOnce(t *testing.T) {
	deps, hosts := runtimeFixture(t)
	cfg, err := config.Load(deps.Paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Notifications = true
	if err := config.Save(deps.Paths.Config, cfg); err != nil {
		t.Fatal(err)
	}
	hosts.snapshot.Version = "0.0.41-preview.20260915.1772"
	notices := 0
	deps.Notify = func(context.Context, string) error { notices++; return nil }
	for i := 0; i < 11; i++ {
		if code := Run(context.Background(), []string{"--scheduled-check"}, strings.NewReader(""), io.Discard, io.Discard, deps); code != 0 {
			t.Fatal(code)
		}
	}
	if notices != 1 || !reflect.DeepEqual(hosts.calls, []string{"local:probe", "remote:probe"}) {
		t.Fatalf("notices %d calls %v", notices, hosts.calls)
	}
}
func TestHelpAndVersionNeedNoEnrollment(t *testing.T) {
	deps, _ := runtimeFixture(t)
	deps.Hosts = nil
	deps.Paths.Config = filepath.Join(t.TempDir(), "missing.json")
	for _, arg := range []string{"--help", "--version"} {
		if code := Run(context.Background(), []string{arg}, strings.NewReader(""), io.Discard, io.Discard, deps); code != 0 {
			t.Fatal(code)
		}
	}
}

func TestRollbackUsesRequestedAliasAndHonorsWholeMachineExclusion(t *testing.T) {
	deps, hosts := runtimeFixture(t)
	cfg, err := config.Load(deps.Paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Devices[1].MachineID = "local"
	if err := config.Save(deps.Paths.Config, cfg); err != nil {
		t.Fatal(err)
	}
	if code := Run(context.Background(), []string{"--rollback", "remote"}, strings.NewReader(""), io.Discard, io.Discard, deps); code != 0 {
		t.Fatal(code)
	}
	if !reflect.DeepEqual(hosts.calls, []string{"remote:rollback"}) {
		t.Fatal(hosts.calls)
	}
	hosts.calls = nil
	if code := Run(context.Background(), []string{"--rollback", "remote", "--exclude", "local"}, strings.NewReader(""), io.Discard, io.Discard, deps); code == 0 {
		t.Fatal("excluded rollback accepted")
	}
	if len(hosts.calls) > 0 {
		t.Fatal(hosts.calls)
	}
}
func TestCheckNeverCallsUnknownInstallationCurrent(t *testing.T) {
	deps, hosts := runtimeFixture(t)
	hosts.snapshot.Supported = false
	hosts.snapshot.Version = ""
	var out bytes.Buffer
	if code := Run(context.Background(), []string{"--check"}, strings.NewReader(""), &out, &out, deps); code != 3 || strings.Contains(out.String(), "are current") {
		t.Fatalf("%d %s", code, out.String())
	}
}

func TestStatusShowsOtherDevicesWhenOneIsUnavailable(t *testing.T) {
	deps, hosts := runtimeFixture(t)
	hosts.fail = "local:status"
	var out bytes.Buffer
	code := Run(context.Background(), []string{"--status"}, strings.NewReader(""), &out, &out, deps)
	if code == 0 || !strings.Contains(out.String(), "remote: CLI") {
		t.Fatalf("%d %s", code, out.String())
	}
	if !reflect.DeepEqual(hosts.calls, []string{"local:status", "remote:status"}) {
		t.Fatal(hosts.calls)
	}
}

func TestRollbackReportsDeferredExit(t *testing.T) {
	deps, hosts := runtimeFixture(t)
	hosts.fail = "local:rollback"
	hosts.snapshot.Deferred = true
	if code := Run(context.Background(), []string{"--rollback", "local"}, strings.NewReader(""), io.Discard, io.Discard, deps); code != 3 {
		t.Fatal(code)
	}
}
