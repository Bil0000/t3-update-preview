package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSelectKeepsRoutesAndExcludesKnownMachineAliases(t *testing.T) {
	cfg := Config{Version: 1, Devices: []Device{
		{ID: "a", Name: "server", Kind: "ssh", Host: "a", MachineID: "machine"},
		{ID: "b", Name: "server", Kind: "relay", URL: "wss://example.test", MachineID: "machine"},
		{ID: "local", Name: "local", Kind: "local"},
	}}
	for _, name := range []string{"unknown", "server"} {
		if _, err := Select(cfg, []string{name}); err == nil {
			t.Fatalf("accepted unknown or ambiguous exclusion %q", name)
		}
	}
	selected, err := Select(cfg, nil)
	if err != nil || len(selected) != 3 || selected[0].ID != "a" || selected[1].ID != "b" {
		t.Fatalf("enrolled routes were lost: %v, %v", selected, err)
	}
	selected, err = Select(cfg, []string{"b"})
	if err != nil || len(selected) != 1 || selected[0].ID != "local" {
		t.Fatalf("alias exclusion: %v, %v", selected, err)
	}
	cfg.Devices[1].Excluded = true
	selected, err = Select(cfg, nil)
	if err != nil || len(selected) != 1 || selected[0].ID != "local" {
		t.Fatalf("saved alias exclusion: %v, %v", selected, err)
	}
	cfg.Devices[2].Name = "a"
	if _, err := Select(cfg, []string{"a"}); err == nil {
		t.Fatal("ID/name ambiguity accepted")
	}
}

func TestValidateUnsafeInputs(t *testing.T) {
	for _, endpoint := range []string{
		"http://example.test", "ws://example.test", "https://user:secret@example.test",
		"https://example.test?token=secret", "https://example.test?", "https://example.test#secret",
		"https://example.test#", "https:///missing", "file:///tmp/server", "http://localhost.evil.test",
	} {
		cfg := Config{Version: 1, Devices: []Device{{ID: "server", Name: "server", Kind: "direct", URL: endpoint}}}
		if err := Validate(cfg); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe endpoint accepted or leaked: %q, %v", endpoint, err)
		}
	}
	for _, host := range []string{"-oProxyCommand=bad", "host;touch", "user@host", "host\nother", "host name", "*"} {
		cfg := Config{Version: 1, Devices: []Device{{ID: "server", Name: "server", Kind: "ssh", Host: host}}}
		if err := Validate(cfg); err == nil {
			t.Fatalf("unsafe SSH alias accepted: %q", host)
		}
	}
	for _, endpoint := range []string{"https://example.test/path", "wss://example.test", "http://127.0.0.1:3773", "ws://[::1]:3773", "http://localhost"} {
		if err := Validate(Config{Version: 1, Devices: []Device{{ID: "server", Name: "server", Kind: "relay", Host: "recovery", URL: endpoint}}}); err != nil {
			t.Fatalf("safe endpoint rejected: %v", err)
		}
	}
	for _, id := range []string{"../escape", ".", "..", "/abs", "-flag"} {
		if err := Validate(Config{Version: 1, Devices: []Device{{ID: id, Name: "local", Kind: "local"}}}); err == nil {
			t.Fatalf("unsafe ID accepted: %q", id)
		}
	}
	if err := Validate(Config{Version: 1, Devices: []Device{{ID: "A", Name: "first", Kind: "local"}, {ID: "a", Name: "second", Kind: "local"}}}); err == nil {
		t.Fatal("case-insensitive filesystem ID collision accepted")
	}
}

func TestSavePrivateAtomicConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "path with spaces", "config.json")
	cfg := Config{Version: 1, Devices: []Device{{ID: "local", Name: "local", Kind: "local"}}}
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	for path, mode := range map[string]os.FileMode{path: 0600, filepath.Dir(path): 0700} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("wrong mode: %v %v", info, err)
		}
	}
	old, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	cfg.Notifications = true
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	oldInfo, _ := old.Stat()
	newInfo, _ := os.Stat(path)
	if os.SameFile(oldInfo, newInfo) {
		t.Fatal("save overwrote the old inode instead of atomic replacement")
	}
	loaded, err := Load(path)
	if err != nil || !reflect.DeepEqual(loaded, cfg) {
		t.Fatalf("round trip: %#v %v", loaded, err)
	}
	before, _ := os.ReadFile(path)
	bad := cfg
	bad.Version = 9
	if Save(path, bad) == nil {
		t.Fatal("invalid save accepted")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("failed save changed existing config")
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temporary files leaked: %v", entries)
	}
	link := filepath.Join(filepath.Dir(path), "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if Save(link, cfg) == nil {
		t.Fatal("saved through a symlink")
	}
	if _, err := Load(link); err == nil {
		t.Fatal("loaded through a symlink")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("loaded public config")
	}
}

func TestLoadStrictJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	for _, body := range []string{`{"version":1,"token":"secret"}`, `{"version":1} {}`, `{"version":1`, `{"version":1}` + strings.Repeat(" ", 1<<20)} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("invalid config accepted or leaked: %v", err)
		}
	}
}

func TestDefaultPathsUseSuppliedEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	paths, err := DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.Config, paths.State} {
		if !strings.HasPrefix(path, home+string(filepath.Separator)) {
			t.Fatalf("path escaped supplied home: %q", path)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("path resolution touched disk: %v", err)
		}
	}
}

func TestSaveRejectsDirectorySymlinkAndOversizedConfig(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Version: 1, Devices: []Device{{ID: "local", Name: "local", Kind: "local"}}}
	if err := Save(filepath.Join(link, "config.json"), cfg); err == nil {
		t.Fatal("saved through a directory symlink")
	}
	cfg.Devices[0].Name = strings.Repeat("a", 1<<20)
	if err := Save(filepath.Join(dir, "config.json"), cfg); err == nil {
		t.Fatal("oversized config accepted")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("rejected save wrote data: %v %v", entries, err)
	}
}
