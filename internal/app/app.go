package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/Bil0000/t3-update-preview/internal/check"
	"github.com/Bil0000/t3-update-preview/internal/config"
	"github.com/Bil0000/t3-update-preview/internal/coordinator"
	"github.com/Bil0000/t3-update-preview/internal/discovery"
	"github.com/Bil0000/t3-update-preview/internal/host"
	"github.com/Bil0000/t3-update-preview/internal/release"
	"github.com/Bil0000/t3-update-preview/internal/schedule"
)

type Dependencies struct {
	Paths      config.Paths
	Hosts      coordinator.Hosts
	Latest     func(context.Context) (release.Release, error)
	Notify     func(context.Context, string) error
	Schedule   func(context.Context, string, string, string, bool) error
	Discover   func() ([]config.Device, error)
	Executable func() (string, error)
	Now        func() time.Time
	Version    string
}

func DefaultDependencies() (Dependencies, error) {
	paths, err := config.DefaultPaths()
	if err != nil {
		return Dependencies{}, err
	}
	runner := func(ctx context.Context, name string, args ...string) error {
		return exec.CommandContext(ctx, name, args...).Run()
	}
	return Dependencies{
		Paths: paths, Hosts: host.Client{}, Now: time.Now, Executable: os.Executable,
		Discover: func() ([]config.Device, error) {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			return discovery.Candidates(home)
		},
		Latest: func(ctx context.Context) (release.Release, error) {
			return release.Latest(ctx, &http.Client{Timeout: 30 * time.Second})
		},
		Notify: func(ctx context.Context, v string) error { return schedule.Notify(ctx, runtime.GOOS, v, runner) },
		Schedule: func(ctx context.Context, exe, state, configPath string, install bool) error {
			if install {
				return schedule.Install(ctx, exe, state, configPath, runner)
			}
			return schedule.Remove(ctx, exe, state, configPath, runner)
		},
	}, nil
}

type repeated []string

func (r *repeated) String() string     { return fmt.Sprint([]string(*r)) }
func (r *repeated) Set(v string) error { *r = append(*r, v); return nil }

type options struct {
	setup, check, status, dryRun, force, scheduled, showVersion bool
	configPath, stateDir, importPath, rollback                  string
	exclude                                                     repeated
	wait                                                        time.Duration
}

func parse(args []string, paths config.Paths, out io.Writer) (options, error) {
	var opts options
	flags := flag.NewFlagSet("t3-update-preview", flag.ContinueOnError)
	flags.SetOutput(out)
	flags.BoolVar(&opts.setup, "setup", false, "enroll devices and configure update notices")
	flags.BoolVar(&opts.check, "check", false, "check releases without updating")
	flags.BoolVar(&opts.status, "status", false, "show enrolled installation and recovery status")
	flags.BoolVar(&opts.dryRun, "dry-run", false, "inspect the update plan without changing T3")
	flags.BoolVar(&opts.force, "force", false, "allow interruption of active T3 work; safety checks remain")
	flags.BoolVar(&opts.scheduled, "scheduled-check", false, "run one due background check")
	flags.BoolVar(&opts.showVersion, "version", false, "show this updater's version")
	flags.StringVar(&opts.configPath, "config", paths.Config, "configuration file")
	flags.StringVar(&opts.stateDir, "state-dir", paths.State, "checker state directory")
	flags.StringVar(&opts.importPath, "import", "", "device list to offer during setup")
	flags.StringVar(&opts.rollback, "rollback", "", "recover one enrolled device from its retained backup")
	flags.DurationVar(&opts.wait, "wait", 30*time.Minute, "maximum idle wait before deferring")
	flags.Var(&opts.exclude, "exclude", "exclude an enrolled device name or ID; repeatable")
	flags.Usage = func() {
		fmt.Fprint(out, "Usage: t3-update-preview [options]\n\nUpdate enrolled T3 preview installations. Run --setup first.\n\n")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return opts, err
	}
	if flags.NArg() != 0 {
		return opts, errors.New("unexpected positional arguments")
	}
	modes := 0
	for _, set := range []bool{opts.setup, opts.check, opts.status, opts.dryRun, opts.scheduled, opts.showVersion, opts.rollback != ""} {
		if set {
			modes++
		}
	}
	if modes > 1 {
		return opts, errors.New("choose only one operation")
	}
	if opts.importPath != "" && !opts.setup {
		return opts, errors.New("--import requires --setup")
	}
	if opts.force && (modes > 0 && opts.rollback == "") {
		return opts, errors.New("--force only applies to update or rollback")
	}
	if opts.wait < 0 {
		return opts, errors.New("--wait cannot be negative")
	}
	if !filepath.IsAbs(opts.configPath) || !filepath.IsAbs(opts.stateDir) {
		return opts, errors.New("configuration and state paths must be absolute")
	}
	return opts, nil
}

func Run(ctx context.Context, args []string, in io.Reader, out, stderr io.Writer, deps Dependencies) int {
	opts, err := parse(args, deps.Paths, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if opts.showVersion {
		fmt.Fprintln(out, "t3-update-preview", deps.Version)
		return 0
	}
	if opts.setup {
		err = setup(ctx, opts, in, out, deps)
	} else {
		err = execute(ctx, opts, out, deps)
	}
	if err == nil {
		return 0
	}
	fmt.Fprintln(stderr, err)
	if errors.Is(err, coordinator.ErrBlocked) {
		return 3
	}
	return 1
}

func execute(ctx context.Context, opts options, out io.Writer, deps Dependencies) error {
	cfg, err := config.Load(opts.configPath)
	if os.IsNotExist(err) {
		return errors.New("no configuration; run t3-update-preview --setup")
	}
	if err != nil {
		return err
	}

	devices, err := config.Select(cfg, opts.exclude)
	if err != nil {
		return err
	}
	if opts.rollback != "" {
		return rollback(ctx, devices, opts, deps.Hosts, out)
	}
	if len(devices) == 0 {
		fmt.Fprintln(out, "No devices selected.")
		return nil
	}
	if opts.scheduled {
		if !cfg.Notifications {
			return nil
		}
		_, err := check.Run(ctx, opts.stateDir, deps.Now(), func(ctx context.Context) (string, error) {
			targets, err := coordinator.Inspect(ctx, devices, deps.Hosts)
			if err != nil {
				return "", err
			}
			selected, err := deps.Latest(ctx)
			if err != nil {
				return "", err
			}
			return available(targets, selected)
		}, deps.Notify)
		return err
	}
	if opts.status {
		var failures error
		for _, device := range devices {
			snapshot, statusErr := deps.Hosts.Call(ctx, device, host.Request{Action: "status"})
			if statusErr != nil {
				fmt.Fprintf(out, "%s: unavailable: %v\n", device.ID, statusErr)
				failures = errors.Join(failures, statusErr)
				continue
			}
			fmt.Fprintf(out, "%s: CLI %s, activity %s\n", device.ID, snapshot.Version, snapshot.Busy)
			for _, field := range [][2]string{{"Service", snapshot.ServiceVersion}, {"Desktop", snapshot.DesktopVersion}, {"Running server", snapshot.RuntimeVersion}, {"Update state", snapshot.Status}, {"Backup", snapshot.Backup}} {
				if field[1] != "" {
					fmt.Fprintf(out, "  %s: %s\n", field[0], field[1])
				}
			}
			if !snapshot.Supported || snapshot.Status == "recovery-required" {
				failures = errors.Join(failures, fmt.Errorf("%w: %s: %s %s", coordinator.ErrBlocked, device.ID, snapshot.Status, snapshot.Blocker))
			}
		}
		return failures
	}
	targets, err := coordinator.Inspect(ctx, devices, deps.Hosts)
	if err != nil {
		return err
	}
	selected, err := deps.Latest(ctx)
	if err != nil {
		return err
	}
	if opts.check {
		v, err := available(targets, selected)
		if err != nil {
			return err
		}
		if v == "" {
			fmt.Fprintln(out, "All selected installations are current.")
		} else {
			fmt.Fprintf(out, "Preview %s is available. Run t3-update-preview to update.\n", v)
		}
		return nil
	}
	targets, err = coordinator.Plan(targets, selected)
	if err != nil {
		return err
	}
	return coordinator.Apply(ctx, targets, selected.Version(), coordinator.Options{Force: opts.force, DryRun: opts.dryRun, Wait: opts.wait}, deps.Hosts, out)
}

func available(targets []coordinator.Target, selected release.Release) (string, error) {
	for _, t := range targets {
		if !t.Snapshot.Supported || t.Snapshot.Version == "" {
			return "", fmt.Errorf("%w: %s: installation could not be checked: %s", coordinator.ErrBlocked, t.Device.ID, t.Snapshot.Blocker)
		}
		for _, v := range []string{t.Snapshot.Version, t.Snapshot.DesktopVersion, t.Snapshot.ServiceVersion, t.Snapshot.RuntimeVersion} {
			if v == "" {
				continue
			}
			order, err := release.Compare(v, selected.Version())
			if err != nil {
				return "", err
			}
			if order < 0 {
				return selected.Version(), nil
			}
		}
	}
	return "", nil
}

func rollback(ctx context.Context, devices []config.Device, opts options, hosts coordinator.Hosts, out io.Writer) error {
	var matches []config.Device
	for _, d := range devices {
		if d.ID == opts.rollback || d.Name == opts.rollback {
			matches = append(matches, d)
		}
	}
	if len(matches) != 1 {
		return errors.New("rollback must name one non-excluded enrolled device")
	}
	snapshot, err := hosts.Call(ctx, matches[0], host.Request{Action: "rollback", Force: opts.force})
	if err != nil {
		if snapshot.Deferred {
			return fmt.Errorf("%w: %v", coordinator.ErrBlocked, err)
		}
		return err
	}
	if snapshot.Status != "healthy" {
		return errors.New("rollback did not pass health checks")
	}
	fmt.Fprintf(out, "%s: recovered %s\n", matches[0].ID, snapshot.Version)
	return nil
}
