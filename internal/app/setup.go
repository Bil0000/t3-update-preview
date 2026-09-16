package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Bil0000/t3-update-preview/internal/config"
	"github.com/Bil0000/t3-update-preview/internal/coordinator"
	"github.com/Bil0000/t3-update-preview/internal/discovery"
	"github.com/Bil0000/t3-update-preview/internal/host"
)

func setup(ctx context.Context, opts options, in io.Reader, out io.Writer, deps Dependencies) error {
	saved, err := config.Load(opts.configPath)
	if os.IsNotExist(err) {
		saved = config.Config{Version: 1}
	} else if err != nil {
		return err
	}
	available := append([]config.Device(nil), saved.Devices...)
	discovered, err := deps.Discover()
	if err != nil {
		fmt.Fprintln(out, "SSH discovery unavailable. Use saved devices or an explicit --import file.")
		discovered = []config.Device{{ID: "local", Name: "local", Kind: "local"}}
	}
	for _, device := range discovered {
		found := false
		for _, old := range available {
			if old.ID == device.ID {
				found = true
				break
			}
		}
		if !found {
			available = append(available, device)
		}
	}
	if opts.importPath != "" {
		file, err := os.Open(opts.importPath)
		if err != nil {
			return err
		}
		imported, err := discovery.Import(file)
		file.Close()
		if err != nil {
			return err
		}
		for _, device := range imported {
			found := false
			for i, old := range available {
				if old.ID == device.ID {
					device.Excluded = device.Excluded || old.Excluded
					if device.Kind == old.Kind && device.Host == old.Host && device.URL == old.URL {
						if device.BaseDir == "" {
							device.BaseDir = old.BaseDir
						}
						if device.BaseDir == old.BaseDir {
							device.MachineID = old.MachineID
						}
					}
					available[i] = device
					found = true
					break
				}
			}
			if !found {
				available = append(available, device)
			}
		}
	}
	if err := config.Validate(config.Config{Version: 1, Devices: available}); err != nil {
		return err
	}
	for _, device := range available {
		fmt.Fprintf(out, "%s: %s (%s, excluded=%t)\n", device.ID, device.Name, device.Kind, device.Excluded)
	}
	scanner := bufio.NewScanner(in)
	answer := func(prompt string) (string, error) {
		fmt.Fprint(out, prompt)
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return "", err
			}
			return "", errors.New("setup input ended before all choices were made; configuration unchanged")
		}
		return strings.TrimSpace(scanner.Text()), nil
	}
	selection, err := answer("Devices to enroll (IDs or unique names, comma-separated; Enter keeps saved devices; none clears): ")
	if err != nil {
		return err
	}
	cfg := saved
	if selection != "" {
		cfg.Devices = nil
		if selection != "none" {
			ids, err := setupIDs(available, selection)
			if err != nil {
				return err
			}
			for _, id := range ids {
				for _, device := range available {
					if device.ID == id {
						cfg.Devices = append(cfg.Devices, device)
					}
				}
			}
		}
	}
	selection, err = answer("Saved exclusions (IDs or unique names, comma-separated; Enter keeps saved exclusions; none clears): ")
	if err != nil {
		return err
	}
	if selection != "" {
		var ids []string
		if selection != "none" {
			ids, err = setupIDs(cfg.Devices, selection)
			if err != nil {
				return err
			}
		}
		for i := range cfg.Devices {
			cfg.Devices[i].Excluded = false
			for _, id := range ids {
				if cfg.Devices[i].ID == id {
					cfg.Devices[i].Excluded = true
				}
			}
		}
	}
	for _, exclusion := range opts.exclude {
		ids, err := setupIDs(cfg.Devices, exclusion)
		if err != nil {
			return err
		}
		for i := range cfg.Devices {
			for _, id := range ids {
				if cfg.Devices[i].ID == id {
					cfg.Devices[i].Excluded = true
				}
			}
		}
	}
	notices, err := answer(fmt.Sprintf("Enable hourly update notices? (yes/no; Enter keeps %t): ", cfg.Notifications))
	if err != nil {
		return err
	}
	switch strings.ToLower(notices) {
	case "yes", "y":
		cfg.Notifications = true
	case "no", "n":
		cfg.Notifications = false
	case "":
	default:
		return errors.New("notices choice must be yes or no")
	}
	selected, err := config.Select(cfg, nil)
	if err != nil {
		return err
	}
	for _, device := range selected {
		if device.Kind != "local" && device.Host == "" {
			return fmt.Errorf("%w: %s needs an explicit SSH recovery alias in its imported host field", coordinator.ErrBlocked, device.ID)
		}
		snapshot, err := deps.Hosts.Call(ctx, device, host.Request{Action: "probe"})
		if err != nil {
			return fmt.Errorf("%s: enrollment probe failed: %w", device.ID, err)
		}
		if snapshot.MachineID == "" || device.MachineID != "" && device.MachineID != snapshot.MachineID {
			return fmt.Errorf("%s: enrollment could not verify the saved machine identity", device.ID)
		}
		for i := range cfg.Devices {
			if cfg.Devices[i].ID != device.ID {
				continue
			}
			cfg.Devices[i].MachineID = snapshot.MachineID
			if snapshot.BaseDir != "" {
				cfg.Devices[i].BaseDir = snapshot.BaseDir
			}
			if snapshot.CLIPath != "" {
				cfg.Devices[i].CLIPath = snapshot.CLIPath
			}
			if snapshot.AppPath != "" {
				cfg.Devices[i].AppPath = snapshot.AppPath
			}
			if snapshot.HealthURL != "" {
				cfg.Devices[i].HealthURL = snapshot.HealthURL
			}
		}
		if !snapshot.Supported {
			fmt.Fprintf(out, "%s: probe found an update blocker: %s\n", device.ID, snapshot.Blocker)
		}
	}
	if err := config.Save(opts.configPath, cfg); err != nil {
		return err
	}
	if cfg.Notifications || saved.Notifications {
		exe, err := deps.Executable()
		if err != nil {
			return fmt.Errorf("devices saved, but schedule setup failed: %w", err)
		}
		if err := deps.Schedule(ctx, exe, opts.stateDir, opts.configPath, cfg.Notifications); err != nil {
			return fmt.Errorf("devices saved, but schedule setup failed: %w", err)
		}
	}
	fmt.Fprintf(out, "Saved %d enrolled devices. Hourly notices: %t.\n", len(cfg.Devices), cfg.Notifications)
	return nil
}

func setupIDs(devices []config.Device, input string) ([]string, error) {
	var ids []string
	seen := make(map[string]bool)
	for _, name := range strings.Split(input, ",") {
		name = strings.TrimSpace(name)
		var matches []string
		for _, device := range devices {
			if device.ID == name || device.Name == name {
				matches = append(matches, device.ID)
			}
		}
		if len(matches) != 1 {
			return nil, errors.New("device choice is unknown or ambiguous; use a unique device ID")
		}
		if !seen[matches[0]] {
			ids = append(ids, matches[0])
			seen[matches[0]] = true
		}
	}
	return ids, nil
}
