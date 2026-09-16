package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"unicode"
)

type Device struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Host      string `json:"host,omitempty"`
	URL       string `json:"url,omitempty"`
	BaseDir   string `json:"base_dir,omitempty"`
	AppPath   string `json:"app_path,omitempty"`
	CLIPath   string `json:"cli_path,omitempty"`
	HealthURL string `json:"health_url,omitempty"`
	MachineID string `json:"machine_id,omitempty"`
	Excluded  bool   `json:"excluded,omitempty"`
}

type Config struct {
	Version       int      `json:"version"`
	Devices       []Device `json:"devices"`
	Notifications bool     `json:"notifications"`
}

type Paths struct {
	Config string
	State  string
}

func DefaultPaths() (Paths, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return Paths{}, err
	}
	stateDir := configDir
	if runtime.GOOS != "darwin" {
		stateDir = os.Getenv("XDG_STATE_HOME")
		if stateDir == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return Paths{}, err
			}
			stateDir = filepath.Join(home, ".local", "state")
		}
	}
	for _, dir := range []string{configDir, stateDir} {
		if !filepath.IsAbs(dir) {
			return Paths{}, errors.New("configuration directories must be absolute paths")
		}
	}
	return Paths{
		Config: filepath.Join(configDir, "t3-update-preview", "config.json"),
		State:  filepath.Join(stateDir, "t3-update-preview"),
	}, nil
}

var safeID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

func Validate(cfg Config) error {
	if cfg.Version != 1 {
		return errors.New("unsupported configuration version; expected 1")
	}
	ids := make(map[string]bool)
	for i, d := range cfg.Devices {
		if !safeID.MatchString(d.ID) || ids[strings.ToLower(d.ID)] {
			return fmt.Errorf("device %d has an invalid or duplicate ID", i+1)
		}
		ids[strings.ToLower(d.ID)] = true
		if strings.TrimSpace(d.Name) == "" || strings.IndexFunc(d.Name, unicode.IsControl) >= 0 {
			return fmt.Errorf("device %s has an invalid name", d.ID)
		}
		if d.Host != "" && !safeID.MatchString(d.Host) {
			return fmt.Errorf("device %s has an invalid SSH alias", d.ID)
		}
		switch d.Kind {
		case "local":
			if d.Host != "" || d.URL != "" {
				return fmt.Errorf("local device %s cannot specify a remote route", d.ID)
			}
		case "ssh":
			if d.Host == "" || d.URL != "" {
				return fmt.Errorf("SSH device %s needs an SSH alias and no direct URL", d.ID)
			}
		case "direct", "relay":
			if d.URL == "" {
				return fmt.Errorf("device %s needs a connection URL", d.ID)
			}
		default:
			return fmt.Errorf("device %s has an unsupported connection kind", d.ID)
		}
		for _, endpoint := range []string{d.URL, d.HealthURL} {
			if endpoint != "" && !validEndpoint(endpoint) {
				return fmt.Errorf("device %s has an unsafe endpoint; use TLS without credentials, query, or fragment", d.ID)
			}
		}
		for _, path := range []string{d.BaseDir, d.AppPath, d.CLIPath} {
			if path != "" && (!filepath.IsAbs(path) || strings.IndexFunc(path, unicode.IsControl) >= 0) {
				return fmt.Errorf("device %s needs absolute installation paths without control characters", d.ID)
			}
		}
		if len(d.MachineID) > 256 || strings.IndexFunc(d.MachineID, unicode.IsControl) >= 0 {
			return fmt.Errorf("device %s has an invalid machine identity", d.ID)
		}
	}
	return nil
}

func validEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(endpoint, "#") || u.Opaque != "" {
		return false
	}
	if u.Scheme == "https" || u.Scheme == "wss" {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	return (u.Scheme == "http" || u.Scheme == "ws") && (strings.EqualFold(u.Hostname(), "localhost") || ip != nil && ip.IsLoopback())
}

func Load(path string) (Config, error) {
	if err := privateDirectory(filepath.Dir(path)); err != nil {
		return Config{}, err
	}
	if err := privateFile(path); err != nil {
		return Config{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer f.Close()
	var cfg Config
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(&cfg); err != nil {
		return Config{}, errors.New("invalid configuration JSON")
	}
	if d.Decode(new(any)) != io.EOF {
		return Config{}, errors.New("unexpected trailing configuration data")
	}
	return cfg, Validate(cfg)
}

func privateFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return errors.New("configuration must be a private regular file (mode 0600)")
	}
	if info.Size() > 1<<20 {
		return errors.New("configuration exceeds 1 MiB")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Uid != uint32(os.Getuid()) {
		return errors.New("configuration must belong to the current user")
	}
	return nil
}

func privateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("configuration directory must be private (mode 0700) and not a symlink")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Uid != uint32(os.Getuid()) {
		return errors.New("configuration directory must belong to the current user")
	}
	return nil
}

func Save(path string, cfg Config) error {
	if err := Validate(cfg); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if len(data)+1 > 1<<20 {
		return errors.New("configuration exceeds 1 MiB")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := privateDirectory(dir); err != nil {
		return err
	}
	if err := privateFile(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := os.CreateTemp(dir, ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	folder, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer folder.Close()
	return folder.Sync()
}

func Select(cfg Config, exclusions []string) ([]Device, error) {
	if err := Validate(cfg); err != nil {
		return nil, err
	}
	excluded := make(map[string]bool)
	for _, d := range cfg.Devices {
		excluded[d.ID] = d.Excluded
	}
	for _, name := range exclusions {
		var matches []string
		for _, d := range cfg.Devices {
			if d.ID == name || d.Name == name {
				matches = append(matches, d.ID)
			}
		}
		if len(matches) == 0 {
			return nil, errors.New("unknown exclusion; use an enrolled device ID or name")
		}
		if len(matches) > 1 {
			return nil, errors.New("ambiguous exclusion; use a unique enrolled device ID")
		}
		excluded[matches[0]] = true
	}
	machines := make(map[string]bool)
	for _, d := range cfg.Devices {
		if excluded[d.ID] && d.MachineID != "" {
			machines[d.MachineID] = true
		}
	}
	var devices []Device
	for _, d := range cfg.Devices {
		if excluded[d.ID] || d.MachineID != "" && machines[d.MachineID] {
			continue
		}
		devices = append(devices, d)
	}
	return devices, nil
}
