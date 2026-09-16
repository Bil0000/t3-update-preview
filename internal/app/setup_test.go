package app

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Bil0000/t3-update-preview/internal/config"
	"github.com/Bil0000/t3-update-preview/internal/host"
)

type setupHosts struct {
	devices   []config.Device
	machineID string
}

func (h *setupHosts) Call(_ context.Context, device config.Device, request host.Request) (host.Snapshot, error) {
	if request.Action != "probe" {
		return host.Snapshot{}, errors.New("setup must only probe")
	}
	h.devices = append(h.devices, device)
	machineID := h.machineID
	if machineID == "" {
		machineID = "machine-" + device.ID
	}
	return host.Snapshot{MachineID: machineID, BaseDir: "/fixture/base", CLIPath: "/fixture/cli", Supported: true}, nil
}

func TestSetupSelectsBeforeProbeAndSavesIdentity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "private", "config.json")
	hosts := &setupHosts{}
	discovered := []config.Device{
		{ID: "local", Name: "local", Kind: "local"},
		{ID: "server", Name: "server", Kind: "ssh", Host: "server"},
		{ID: "new", Name: "new", Kind: "ssh", Host: "new"},
	}
	deps := Dependencies{Hosts: hosts, Discover: func() ([]config.Device, error) { return discovered, nil }}
	if err := setup(context.Background(), options{configPath: path, stateDir: filepath.Join(dir, "state")}, strings.NewReader("local,server\nserver\nno\n"), io.Discard, deps); err != nil {
		t.Fatal(err)
	}
	if len(hosts.devices) != 1 || hosts.devices[0].ID != "local" {
		t.Fatalf("contacted an excluded or unselected device: %v", hosts.devices)
	}
	cfg, err := config.Load(path)
	if err != nil || len(cfg.Devices) != 2 || cfg.Devices[0].MachineID != "machine-local" || cfg.Devices[0].CLIPath != "/fixture/cli" || !cfg.Devices[1].Excluded {
		t.Fatalf("saved config: %v %v", cfg, err)
	}
}

func TestSetupPreservesExclusionsAndRemovesNotices(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "private", "config.json")
	cfg := config.Config{Version: 1, Notifications: true, Devices: []config.Device{
		{ID: "a", Name: "a", Kind: "ssh", Host: "a", MachineID: "machine", Excluded: true},
		{ID: "b", Name: "b", Kind: "ssh", Host: "b", MachineID: "machine"},
	}}
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	hosts := &setupHosts{}
	removed := false
	deps := Dependencies{
		Hosts: hosts,
		Discover: func() ([]config.Device, error) {
			return []config.Device{{ID: "new", Name: "new", Kind: "ssh", Host: "new"}}, nil
		},
		Executable: func() (string, error) { return "/fixture/updater", nil },
		Schedule: func(_ context.Context, exe, state, configPath string, install bool) error {
			if exe != "/fixture/updater" || state != filepath.Join(dir, "state") || configPath != path || install {
				t.Fatal("incorrect schedule removal")
			}
			removed = true
			return nil
		},
	}
	if err := setup(context.Background(), options{configPath: path, stateDir: filepath.Join(dir, "state")}, strings.NewReader("\n\nno\n"), io.Discard, deps); err != nil {
		t.Fatal(err)
	}
	if len(hosts.devices) != 0 || !removed {
		t.Fatalf("excluded aliases contacted or schedule not removed: %v %t", hosts.devices, removed)
	}
	saved, err := config.Load(path)
	if err != nil || saved.Notifications || !reflect.DeepEqual(saved.Devices, cfg.Devices) {
		t.Fatalf("saved enrollment changed: %v %v", saved, err)
	}
}

func TestSetupRejectsChoicesWithoutProbesOrSaving(t *testing.T) {
	for _, input := range []string{"unknown\n", "same\n", "local\nunknown\n", "local\n\nmaybe\n", "local\n"} {
		t.Run(strings.ReplaceAll(input, "\n", "_"), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "private", "config.json")
			hosts := &setupHosts{}
			deps := Dependencies{Hosts: hosts, Discover: func() ([]config.Device, error) {
				return []config.Device{{ID: "local", Name: "same", Kind: "local"}, {ID: "server", Name: "same", Kind: "ssh", Host: "server"}}, nil
			}}
			if err := setup(context.Background(), options{configPath: path}, strings.NewReader(input), io.Discard, deps); err == nil {
				t.Fatal("invalid choice accepted")
			}
			if len(hosts.devices) != 0 {
				t.Fatal("probe happened before selection validation")
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("invalid setup wrote config")
			}
		})
	}
}

func TestSetupImportsExplicitRecoveryRouteAndReportsScheduleFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "private", "config.json")
	importPath := filepath.Join(dir, "devices.json")
	if err := os.WriteFile(importPath, []byte(`[{"id":"relay","name":"relay","kind":"relay","url":"wss://example.test","host":"recovery","base_dir":"/chosen/base","app_path":"/chosen/App.app"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	hosts := &setupHosts{}
	deps := Dependencies{
		Hosts:      hosts,
		Discover:   func() ([]config.Device, error) { return nil, errors.New("unavailable") },
		Executable: func() (string, error) { return "/fixture/updater", nil },
		Schedule: func(_ context.Context, _, _, _ string, install bool) error {
			if !install {
				t.Fatal("expected notice installation")
			}
			return errors.New("fixture scheduler unavailable")
		},
	}
	err := setup(context.Background(), options{configPath: path, importPath: importPath, stateDir: filepath.Join(dir, "state")}, strings.NewReader("relay\n\nyes\n"), io.Discard, deps)
	if err == nil || !strings.Contains(err.Error(), "devices saved, but schedule setup failed") {
		t.Fatalf("wrong schedule failure: %v", err)
	}
	if len(hosts.devices) != 1 || hosts.devices[0].BaseDir != "/chosen/base" || hosts.devices[0].Host != "recovery" {
		t.Fatalf("imported paths/route not passed to probe: %v", hosts.devices)
	}
	saved, err := config.Load(path)
	if err != nil || saved.Devices[0].AppPath != "/chosen/App.app" {
		t.Fatalf("saved config lost explicit app path: %v %v", saved, err)
	}
}

func TestSetupMissingRecoveryRouteBlocksWithoutProbe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "config.json")
	hosts := &setupHosts{}
	deps := Dependencies{Hosts: hosts, Discover: func() ([]config.Device, error) {
		return []config.Device{{ID: "relay", Name: "relay", Kind: "relay", URL: "wss://example.test"}}, nil
	}}
	err := setup(context.Background(), options{configPath: path}, strings.NewReader("relay\n\nno\n"), io.Discard, deps)
	if err == nil || !strings.Contains(err.Error(), "explicit SSH recovery alias") || len(hosts.devices) != 0 {
		t.Fatalf("missing recovery route was not blocked before probe: %v %v", err, hosts.devices)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("blocked enrollment wrote config")
	}
}

func TestSetupImportPreservesKnownExcludedAliases(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "private", "config.json")
	cfg := config.Config{Version: 1, Devices: []config.Device{
		{ID: "a", Name: "a", Kind: "ssh", Host: "a", MachineID: "shared", Excluded: true},
		{ID: "b", Name: "b", Kind: "ssh", Host: "b", MachineID: "shared", BaseDir: "/saved/base"},
	}}
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	importPath := filepath.Join(dir, "devices.json")
	if err := os.WriteFile(importPath, []byte(`[{"id":"b","name":"b","kind":"ssh","host":"b"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	hosts := &setupHosts{}
	deps := Dependencies{Hosts: hosts, Discover: func() ([]config.Device, error) { return nil, nil }}
	if err := setup(context.Background(), options{configPath: path, importPath: importPath}, strings.NewReader("a,b\n\nno\n"), io.Discard, deps); err != nil {
		t.Fatal(err)
	}
	if len(hosts.devices) != 0 {
		t.Fatalf("import caused contact with an excluded machine: %v", hosts.devices)
	}
	saved, err := config.Load(path)
	if err != nil || saved.Devices[1].MachineID != "shared" || saved.Devices[1].BaseDir != "/saved/base" {
		t.Fatalf("known route lost saved identity/base: %v %v", saved, err)
	}
}

func TestSetupChangedImportDoesNotReuseIdentity(t *testing.T) {
	for _, imported := range []string{
		`[{"id":"b","name":"b","kind":"ssh","host":"replacement"}]`,
		`[{"id":"b","name":"b","kind":"ssh","host":"b","base_dir":"/changed/base"}]`,
	} {
		dir := t.TempDir()
		path := filepath.Join(dir, "private", "config.json")
		cfg := config.Config{Version: 1, Devices: []config.Device{{ID: "b", Name: "b", Kind: "ssh", Host: "b", MachineID: "previous", BaseDir: "/saved/base"}}}
		if err := config.Save(path, cfg); err != nil {
			t.Fatal(err)
		}
		importPath := filepath.Join(dir, "devices.json")
		if err := os.WriteFile(importPath, []byte(imported), 0600); err != nil {
			t.Fatal(err)
		}
		hosts := &setupHosts{}
		deps := Dependencies{Hosts: hosts, Discover: func() ([]config.Device, error) { return nil, nil }}
		if err := setup(context.Background(), options{configPath: path, importPath: importPath}, strings.NewReader("b\n\nno\n"), io.Discard, deps); err != nil {
			t.Fatal(err)
		}
		if len(hosts.devices) != 1 || hosts.devices[0].MachineID != "" {
			t.Fatalf("changed route reused saved identity: %v", hosts.devices)
		}
	}
}

func TestSetupProbesEveryNonexcludedAlias(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "config.json")
	cfg := config.Config{Version: 1, Devices: []config.Device{
		{ID: "a", Name: "a", Kind: "ssh", Host: "a", MachineID: "shared"},
		{ID: "b", Name: "b", Kind: "direct", Host: "a", URL: "https://example.test", MachineID: "shared"},
	}}
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	hosts := &setupHosts{machineID: "shared"}
	deps := Dependencies{Hosts: hosts, Discover: func() ([]config.Device, error) { return nil, nil }}
	if err := setup(context.Background(), options{configPath: path}, strings.NewReader("\n\nno\n"), io.Discard, deps); err != nil {
		t.Fatal(err)
	}
	if len(hosts.devices) != 2 || hosts.devices[0].ID != "a" || hosts.devices[1].ID != "b" {
		t.Fatalf("setup skipped an enrolled alias: %v", hosts.devices)
	}
	saved, err := config.Load(path)
	if err != nil || len(saved.Devices) != 2 || saved.Devices[0].MachineID != "shared" || saved.Devices[1].MachineID != "shared" {
		t.Fatalf("aliases lost their verified identity: %v %v", saved, err)
	}
}
