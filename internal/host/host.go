package host

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/Bil0000/t3-update-preview/internal/config"
	"github.com/Bil0000/t3-update-preview/internal/native"
	"github.com/Bil0000/t3-update-preview/internal/release"
)

//go:embed worker.py
var worker string

type Request struct {
	Action            string          `json:"action"`
	BaseDir           string          `json:"base_dir,omitempty"`
	CLIPath           string          `json:"cli_path,omitempty"`
	AppPath           string          `json:"app_path,omitempty"`
	HealthURL         string          `json:"health_url,omitempty"`
	Target            string          `json:"target,omitempty"`
	Assets            []release.Asset `json:"assets,omitempty"`
	Force             bool            `json:"force,omitempty"`
	Operation         string          `json:"operation,omitempty"`
	ExpectedMachineID string          `json:"expected_machine_id,omitempty"`
}

type Snapshot struct {
	MachineID        string `json:"machine_id"`
	OS               string `json:"os"`
	Arch             string `json:"arch"`
	Version          string `json:"version"`
	DesktopVersion   string `json:"desktop_version"`
	CLIPath          string `json:"cli_path"`
	AppPath          string `json:"app_path"`
	BaseDir          string `json:"base_dir"`
	HealthURL        string `json:"health_url"`
	ServiceInstalled bool   `json:"service_installed"`
	ServiceVersion   string `json:"service_version"`
	RuntimeVersion   string `json:"runtime_version"`
	EnvironmentID    string `json:"environment_id"`
	ServiceRunning   bool   `json:"service_running"`
	DesktopRunning   bool   `json:"desktop_running"`
	Busy             string `json:"busy"`
	Supported        bool   `json:"supported"`
	Blocker          string `json:"blocker"`
	Status           string `json:"status"`
	Backup           string `json:"backup"`
	Error            string `json:"error"`
	Deferred         bool   `json:"deferred"`
}

type Client struct{}

func (Client) Call(ctx context.Context, device config.Device, request Request) (Snapshot, error) {
	if err := config.Validate(config.Config{Version: 1, Devices: []config.Device{device}}); err != nil {
		return Snapshot{}, err
	}
	request.BaseDir, request.CLIPath, request.AppPath = device.BaseDir, device.CLIPath, device.AppPath
	request.HealthURL = device.HealthURL
	if request.ExpectedMachineID == "" {
		request.ExpectedMachineID = device.MachineID
	}
	remoteURL := device.Kind == "direct" || device.Kind == "relay"
	expectedEnvironment := ""
	if remoteURL && (request.Action == "prepare" || request.Action == "apply") {
		probeRequest := request
		probeRequest.Action = "probe"
		snapshot, err := invoke(ctx, device, probeRequest)
		if err != nil {
			return snapshot, err
		}
		if err := verifyRoute(ctx, device, snapshot); err != nil {
			return snapshot, err
		}
		expectedEnvironment = snapshot.EnvironmentID
	}
	if remoteURL && request.Action == "rollback" && request.ExpectedMachineID == "" {
		return Snapshot{}, errors.New("rollback through a recovery route requires an enrolled machine identity")
	}
	if remoteURL && request.Action == "rollback" {
		probeRequest := request
		probeRequest.Action = "probe"
		snapshot, err := invoke(ctx, device, probeRequest)
		if err != nil {
			return snapshot, err
		}
		if descriptor, err := native.Probe(ctx, device.URL, ""); err == nil && snapshot.EnvironmentID != "" && descriptor.EnvironmentID != snapshot.EnvironmentID {
			return snapshot, fmt.Errorf("%s: original T3 URL and recovery host identify different environments", device.ID)
		}
	}
	snapshot, err := invoke(ctx, device, request)
	if err != nil {
		return snapshot, err
	}
	if remoteURL && request.Action == "apply" && !snapshot.Deferred {
		if snapshot.EnvironmentID != expectedEnvironment || (request.Target != "" && snapshot.RuntimeVersion != request.Target) {
			return snapshot, errors.New("host apply completed but the running T3 identity or target version could not be verified")
		}
	}
	if remoteURL && (request.Action == "probe" || request.Action == "apply" || request.Action == "rollback") {
		if err := verifyRoute(ctx, device, snapshot); err != nil {
			if request.Action == "apply" || request.Action == "rollback" {
				return snapshot, fmt.Errorf("host %s completed; original T3 route remains unverified: %w", request.Action, err)
			}
			return snapshot, err
		}
	}
	return snapshot, nil
}

func verifyRoute(ctx context.Context, device config.Device, snapshot Snapshot) error {
	if snapshot.EnvironmentID == "" || snapshot.RuntimeVersion == "" {
		return fmt.Errorf("%s: recovery host did not supply the running T3 environment identity and version", device.ID)
	}
	descriptor, err := native.Probe(ctx, device.URL, snapshot.RuntimeVersion)
	if err != nil {
		return fmt.Errorf("%s: original T3 route: %w", device.ID, err)
	}
	if descriptor.EnvironmentID != snapshot.EnvironmentID {
		return fmt.Errorf("%s: original T3 URL and recovery host identify different environments", device.ID)
	}
	return nil
}

func invoke(ctx context.Context, device config.Device, request Request) (Snapshot, error) {
	input, err := json.Marshal(request)
	if err != nil {
		return Snapshot{}, err
	}
	name, args, err := command(device)
	if err != nil {
		return Snapshot{}, err
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr limitedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	var snapshot Snapshot
	if err := json.Unmarshal(stdout.Bytes(), &snapshot); err != nil {
		if ctx.Err() != nil {
			return snapshot, ctx.Err()
		}
		return snapshot, fmt.Errorf("%s: host worker did not return a valid response; check Python 3 and SSH access (%v)", device.ID, runErr)
	}
	if snapshot.Error != "" {
		return snapshot, errors.New(snapshot.Error)
	}
	if runErr != nil {
		return snapshot, fmt.Errorf("%s: host worker failed: %w", device.ID, runErr)
	}
	return snapshot, nil
}

func command(device config.Device) (string, []string, error) {
	if device.Kind == "local" {
		return "python3", []string{"-c", worker}, nil
	}
	if device.Host == "" {
		return "", nil, fmt.Errorf("%s: this %s connection needs an enrolled SSH recovery route; no supported standalone T3 credential export is available", device.ID, device.Kind)
	}
	remote := "python3 -c '" + strings.ReplaceAll(worker, "'", "'\"'\"'") + "'"
	return "ssh", []string{"-T", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=10", "--", device.Host, remote}, nil
}

type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	const limit = 1 << 20
	if b.Len()+len(p) > limit {
		return 0, errors.New("host response exceeds 1 MiB")
	}
	return b.Buffer.Write(p)
}
