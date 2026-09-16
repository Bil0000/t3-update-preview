package coordinator

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/Bil0000/t3-update-preview/internal/config"
	"github.com/Bil0000/t3-update-preview/internal/host"
	"github.com/Bil0000/t3-update-preview/internal/release"
)

type Hosts interface {
	Call(context.Context, config.Device, host.Request) (host.Snapshot, error)
}

type Target struct {
	Device   config.Device
	Snapshot host.Snapshot
	Assets   []release.Asset
	Routes   []config.Device
	Current  bool
}

type Options struct {
	Force  bool
	DryRun bool
	Wait   time.Duration
}

var ErrBlocked = errors.New("update blocked")

func Inspect(ctx context.Context, devices []config.Device, hosts Hosts) ([]Target, error) {
	targets := make([]Target, 0, len(devices))
	seen := make(map[string]int)
	for _, device := range devices {
		snapshot, err := hosts.Call(ctx, device, host.Request{Action: "probe"})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", device.ID, err)
		}
		if snapshot.MachineID == "" || device.MachineID != "" && device.MachineID != snapshot.MachineID {
			return nil, fmt.Errorf("%s: machine identity is missing or changed; run setup again", device.ID)
		}
		device.MachineID = snapshot.MachineID
		device.BaseDir, device.CLIPath, device.AppPath = snapshot.BaseDir, snapshot.CLIPath, snapshot.AppPath
		device.HealthURL = snapshot.HealthURL
		if index, exists := seen[snapshot.MachineID]; exists {
			if !snapshot.Supported {
				return nil, fmt.Errorf("%w: %s: %s", ErrBlocked, device.ID, snapshot.Blocker)
			}
			targets[index].Routes = append(targets[index].Routes, device)
			if snapshot.Version != targets[index].Snapshot.Version || snapshot.DesktopVersion != targets[index].Snapshot.DesktopVersion || snapshot.ServiceVersion != targets[index].Snapshot.ServiceVersion || snapshot.RuntimeVersion != targets[index].Snapshot.RuntimeVersion {
				return nil, fmt.Errorf("%s: aliases disagree about the installation", device.ID)
			}
			continue
		}
		seen[snapshot.MachineID] = len(targets)
		targets = append(targets, Target{Device: device, Snapshot: snapshot, Routes: []config.Device{device}})
	}
	slices.SortStableFunc(targets, func(a, b Target) int {
		if a.Device.Kind == "local" && b.Device.Kind != "local" {
			return 1
		}
		if a.Device.Kind != "local" && b.Device.Kind == "local" {
			return -1
		}
		return 0
	})
	return targets, nil
}

func Plan(targets []Target, selected release.Release) ([]Target, error) {
	for i := range targets {
		target := &targets[i]
		if !target.Snapshot.Supported {
			return nil, fmt.Errorf("%w: %s: %s", ErrBlocked, target.Device.ID, target.Snapshot.Blocker)
		}
		cliCurrent, err := current(target.Snapshot.Version, selected.Version())
		if err != nil {
			return nil, fmt.Errorf("%s: %w", target.Device.ID, err)
		}
		appCurrent := true
		if target.Snapshot.AppPath != "" {
			appCurrent, err = current(target.Snapshot.DesktopVersion, selected.Version())
			if err != nil {
				return nil, fmt.Errorf("%s: desktop: %w", target.Device.ID, err)
			}
		}
		serviceCurrent := true
		if target.Snapshot.ServiceInstalled {
			serviceCurrent, err = current(target.Snapshot.ServiceVersion, selected.Version())
			if err != nil {
				return nil, fmt.Errorf("%s: service: %w", target.Device.ID, err)
			}
			serviceCurrent = serviceCurrent && target.Snapshot.ServiceRunning
		}
		runtimeCurrent := true
		if target.Snapshot.DesktopRunning {
			runtimeCurrent, err = current(target.Snapshot.RuntimeVersion, selected.Version())
			if err != nil {
				return nil, fmt.Errorf("%s: running desktop: %w", target.Device.ID, err)
			}
		}
		target.Current = cliCurrent && appCurrent && serviceCurrent && runtimeCurrent
		if target.Current {
			continue
		}
		target.Assets, err = selected.Required(target.Snapshot.OS, target.Snapshot.Arch, target.Snapshot.AppPath != "")
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrBlocked, target.Device.ID, err)
		}
	}
	return targets, nil
}

func current(installed, selected string) (bool, error) {
	if installed == "" {
		return false, nil
	}
	order, err := release.Compare(installed, selected)
	if err != nil {
		return false, err
	}
	if order > 0 {
		return false, errors.New("installed version is newer than the selected preview; refusing downgrade")
	}
	return order == 0, nil
}

func Apply(ctx context.Context, targets []Target, version string, options Options, hosts Hosts, out io.Writer) (err error) {
	for _, target := range targets {
		state := "update"
		if target.Current {
			state = "current"
		}
		fmt.Fprintf(out, "%s: %s → %s (%s)\n", target.Device.ID, target.Snapshot.Version, version, state)
	}
	if options.DryRun {
		return nil
	}
	if err := waitIdle(ctx, targets, options, hosts, out); err != nil {
		return err
	}
	operation := rand.Text()
	var prepared []Target
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		for _, target := range prepared {
			_, cleanupErr := hosts.Call(cleanupCtx, target.Device, host.Request{Action: "cleanup", Operation: operation})
			if cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("%s cleanup: %w", target.Device.ID, cleanupErr))
			}
		}
	}()
	for _, target := range targets {
		if target.Current {
			continue
		}
		prepared = append(prepared, target)
		fmt.Fprintf(out, "%s: preparing verified files\n", target.Device.ID)
		_, err := hosts.Call(ctx, target.Device, host.Request{Action: "prepare", Target: version, Assets: target.Assets, Operation: operation})
		if err != nil {
			return fmt.Errorf("%s preparation failed; no later device was activated: %w", target.Device.ID, err)
		}
	}
	for _, target := range prepared {
		fmt.Fprintf(out, "%s: backing up and applying %s\n", target.Device.ID, version)
		snapshot, err := hosts.Call(ctx, target.Device, host.Request{Action: "apply", Target: version, Assets: target.Assets, Operation: operation, Force: options.Force})
		if err != nil {
			if snapshot.Deferred {
				err = fmt.Errorf("%w: %v", ErrBlocked, err)
			}
			return fmt.Errorf("%s: %w; remaining devices were not activated; inspect --status before retrying", target.Device.ID, err)
		}
		if snapshot.Status != "healthy" || snapshot.Version != version || target.Snapshot.AppPath != "" && snapshot.RuntimeVersion != version || target.Snapshot.AppPath != "" && snapshot.DesktopVersion != version || target.Snapshot.ServiceInstalled && (!snapshot.ServiceRunning || snapshot.ServiceVersion != version) {
			return fmt.Errorf("%s: update did not verify the requested running version; inspect --status", target.Device.ID)
		}
		for _, route := range target.Routes {
			if route.ID == target.Device.ID {
				continue
			}
			verified, routeErr := hosts.Call(ctx, route, host.Request{Action: "probe"})
			if routeErr != nil {
				return fmt.Errorf("%s updated; route %s could not be verified: %w", target.Device.ID, route.ID, routeErr)
			}
			if !verified.Supported || verified.MachineID != target.Device.MachineID || verified.Version != version || target.Snapshot.AppPath != "" && verified.RuntimeVersion != version || target.Snapshot.AppPath != "" && verified.DesktopVersion != version || target.Snapshot.ServiceInstalled && (!verified.ServiceRunning || verified.ServiceVersion != version) {
				return fmt.Errorf("%s updated; route %s returned a different identity or version", target.Device.ID, route.ID)
			}
		}
		fmt.Fprintf(out, "%s: healthy on %s; backup %s\n", target.Device.ID, version, snapshot.Backup)
	}
	return nil
}

func waitIdle(ctx context.Context, targets []Target, options Options, hosts Hosts, out io.Writer) error {
	if options.Force {
		return nil
	}
	deadline := time.Now().Add(options.Wait)
	for {
		busy := false
		for _, target := range targets {
			if target.Current {
				continue
			}
			snapshot, err := hosts.Call(ctx, target.Device, host.Request{Action: "probe"})
			if err != nil {
				return err
			}
			if snapshot.MachineID != target.Device.MachineID {
				return errors.New("device identity changed during idle wait")
			}
			if snapshot.Busy == "unknown" {
				return fmt.Errorf("%w: %s activity cannot be proved idle; use --force only when interruption is acceptable", ErrBlocked, target.Device.ID)
			}
			if snapshot.Busy != "idle" {
				busy = true
			}
		}
		if !busy {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%w: devices are still busy; no updates applied", ErrBlocked)
		}
		fmt.Fprintln(out, "Waiting for active work to finish. Ctrl-C cancels without applying an update.")
		timer := time.NewTimer(min(10*time.Second, time.Until(deadline)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
