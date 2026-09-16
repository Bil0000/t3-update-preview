package coordinator

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/Bil0000/t3-update-preview/internal/config"
	"github.com/Bil0000/t3-update-preview/internal/host"
	"github.com/Bil0000/t3-update-preview/internal/release"
)

const previous = "0.0.41-preview.20260915.1772"
const next = "0.0.41-preview.20260916.1794"

type fakeHosts struct {
	calls     []string
	snapshots map[string]host.Snapshot
	fail      string
	applied   host.Snapshot
}

func (f *fakeHosts) Call(_ context.Context, d config.Device, r host.Request) (host.Snapshot, error) {
	key := d.ID + ":" + r.Action
	f.calls = append(f.calls, key)
	if key == f.fail {
		return host.Snapshot{}, errors.New("fixture failure")
	}
	if r.Action == "apply" {
		return f.applied, nil
	}
	return f.snapshots[d.ID], nil
}
func fixtureTargets() ([]Target, *fakeHosts) {
	targets := []Target{}
	fake := &fakeHosts{snapshots: map[string]host.Snapshot{}, applied: host.Snapshot{Status: "healthy", Version: next}}
	for _, id := range []string{"remote", "local"} {
		d := config.Device{ID: id, Name: id, Kind: id, MachineID: id}
		if id == "remote" {
			d.Kind = "ssh"
			d.Host = "server"
		}
		s := host.Snapshot{MachineID: id, OS: "linux", Arch: "x64", Version: previous, Busy: "idle", Supported: true}
		targets = append(targets, Target{Device: d, Snapshot: s})
		fake.snapshots[id] = s
	}
	return targets, fake
}
func TestPrepareAllBeforeApplyAndCleanup(t *testing.T) {
	targets, fake := fixtureTargets()
	if err := Apply(context.Background(), targets, next, Options{}, fake, io.Discard); err != nil {
		t.Fatal(err)
	}
	want := []string{"remote:probe", "local:probe", "remote:prepare", "local:prepare", "remote:apply", "local:apply", "remote:cleanup", "local:cleanup"}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Fatalf("calls %v", fake.calls)
	}
}
func TestFailureStopsActivation(t *testing.T) {
	for _, failure := range []string{"local:prepare", "remote:apply"} {
		t.Run(failure, func(t *testing.T) {
			targets, fake := fixtureTargets()
			fake.fail = failure
			if err := Apply(context.Background(), targets, next, Options{}, fake, io.Discard); err == nil {
				t.Fatal("accepted failure")
			}
			for _, call := range fake.calls {
				if call == "local:apply" || failure == "local:prepare" && call == "remote:apply" {
					t.Fatalf("activated after failure: %v", fake.calls)
				}
			}
			if fake.calls[len(fake.calls)-1] != "local:cleanup" {
				t.Fatalf("cleanup missing: %v", fake.calls)
			}
		})
	}
}
func TestBusyUnknownAndDryRunDoNotMutate(t *testing.T) {
	for _, busy := range []string{"busy", "unknown"} {
		t.Run(busy, func(t *testing.T) {
			targets, fake := fixtureTargets()
			s := fake.snapshots["remote"]
			s.Busy = busy
			fake.snapshots["remote"] = s
			if err := Apply(context.Background(), targets, next, Options{}, fake, io.Discard); !errors.Is(err, ErrBlocked) {
				t.Fatalf("error %v", err)
			}
			for _, call := range fake.calls {
				if !strings.HasSuffix(call, ":probe") {
					t.Fatal(call)
				}
			}
		})
	}
	targets, fake := fixtureTargets()
	if err := Apply(context.Background(), targets, next, Options{DryRun: true}, fake, io.Discard); err != nil || len(fake.calls) > 0 {
		t.Fatalf("dry run called host: %v %v", fake.calls, err)
	}
}
func TestForceDoesNotBypassFailedPreparation(t *testing.T) {
	targets, fake := fixtureTargets()
	fake.fail = "remote:prepare"
	if err := Apply(context.Background(), targets, next, Options{Force: true}, fake, io.Discard); err == nil {
		t.Fatal("accepted failure")
	}
	if !reflect.DeepEqual(fake.calls, []string{"remote:prepare", "remote:cleanup"}) {
		t.Fatal(fake.calls)
	}
}
func TestHealthIncludesServiceAndDesktop(t *testing.T) {
	targets, fake := fixtureTargets()
	targets = targets[:1]
	targets[0].Snapshot.ServiceInstalled = true
	targets[0].Snapshot.AppPath = "/test/app"
	fake.applied.ServiceRunning = true
	fake.applied.ServiceVersion = previous
	fake.applied.DesktopVersion = next
	if err := Apply(context.Background(), targets, next, Options{Force: true}, fake, io.Discard); err == nil {
		t.Fatal("accepted stale running service")
	}
	fake.applied.ServiceVersion = next
	fake.applied.DesktopVersion = previous
	if err := Apply(context.Background(), targets, next, Options{Force: true}, fake, io.Discard); err == nil {
		t.Fatal("accepted stale desktop")
	}
}
func TestInspectIdentityAndRemoteFirst(t *testing.T) {
	targets, fake := fixtureTargets()
	devices := []config.Device{targets[1].Device, targets[0].Device}
	got, err := Inspect(context.Background(), devices, fake)
	if err != nil || got[0].Device.ID != "remote" {
		t.Fatalf("%v %v", got, err)
	}
	s := fake.snapshots["remote"]
	s.MachineID = "different"
	fake.snapshots["remote"] = s
	if _, err := Inspect(context.Background(), devices, fake); err == nil {
		t.Fatal("accepted identity change")
	}
}
func TestPlanIncludesStaleServiceAndRejectsDowngrade(t *testing.T) {
	targets, _ := fixtureTargets()
	targets = targets[:1]
	targets[0].Snapshot.Version = next
	selected := release.Release{Tag: "v" + next, Assets: []release.Asset{{Name: "t3-" + next + "-linux-x64.tar.gz", Digest: "sha256:" + strings.Repeat("a", 64), Size: 123, URL: "https://github.com/pingdotgg/t3code/releases/download/v" + next + "/t3-" + next + "-linux-x64.tar.gz"}}}
	targets[0].Snapshot.ServiceInstalled = true
	targets[0].Snapshot.ServiceRunning = true
	targets[0].Snapshot.ServiceVersion = previous
	got, err := Plan(targets, selected)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Current {
		t.Fatal("stale service marked current")
	}
	targets[0].Snapshot.Version = "0.0.41-preview.20260917.1800"
	if _, err := Plan(targets, selected); err == nil {
		t.Fatal("accepted downgrade")
	}
}

func TestAliasesActivateOnceAndEachRouteIsRechecked(t *testing.T) {
	targets, fake := fixtureTargets()
	devices := []config.Device{targets[0].Device, {ID: "route", Name: "route", Kind: "direct", Host: "server", URL: "https://example.test", MachineID: "remote"}}
	fake.snapshots["route"] = fake.snapshots["remote"]
	grouped, err := Inspect(context.Background(), devices, fake)
	if err != nil {
		t.Fatal(err)
	}
	if len(grouped) != 1 || len(grouped[0].Routes) != 2 {
		t.Fatal(grouped)
	}
	route := fake.snapshots["route"]
	route.Version = next
	fake.snapshots["route"] = route
	fake.calls = nil
	if err := Apply(context.Background(), grouped, next, Options{Force: true}, fake, io.Discard); err != nil {
		t.Fatal(err)
	}
	want := []string{"remote:prepare", "remote:apply", "route:probe", "remote:cleanup"}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Fatal(fake.calls)
	}
}
func TestBrokenAliasCannotReportSuccess(t *testing.T) {
	targets, fake := fixtureTargets()
	targets = targets[:1]
	targets[0].Routes = []config.Device{targets[0].Device, {ID: "broken", MachineID: "remote"}}
	fake.fail = "broken:probe"
	if err := Apply(context.Background(), targets, next, Options{Force: true}, fake, io.Discard); err == nil || !strings.Contains(err.Error(), "route broken") {
		t.Fatal(err)
	}
}

type deferredHosts struct{ *fakeHosts }

func (f deferredHosts) Call(ctx context.Context, d config.Device, r host.Request) (host.Snapshot, error) {
	if r.Action == "apply" {
		return host.Snapshot{Deferred: true}, errors.New("work started after preparation")
	}
	return f.fakeHosts.Call(ctx, d, r)
}
func TestLateBusyStateIsDeferred(t *testing.T) {
	targets, fake := fixtureTargets()
	err := Apply(context.Background(), targets, next, Options{}, deferredHosts{fake}, io.Discard)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("deferred status lost: %v", err)
	}
}

func TestPlanIncludesOldRunningDesktop(t *testing.T) {
	targets, _ := fixtureTargets()
	targets = targets[:1]
	s := &targets[0].Snapshot
	s.Version = next
	s.DesktopVersion = next
	s.AppPath = "/fixture/Desktop.AppImage"
	s.DesktopRunning = true
	s.RuntimeVersion = previous
	selected := release.Release{Tag: "v" + next}
	for _, name := range []string{"t3-" + next + "-linux-x64.tar.gz", "T3-Code-" + next + "-x86_64.AppImage"} {
		selected.Assets = append(selected.Assets, release.Asset{Name: name, URL: "https://github.com/pingdotgg/t3code/releases/download/v" + next + "/" + name, Digest: "sha256:" + strings.Repeat("a", 64), Size: 123})
	}
	got, err := Plan(targets, selected)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Current {
		t.Fatal("old running app marked current")
	}
}
