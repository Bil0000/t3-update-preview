package check

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestCheckSequence(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		initial               State
		version               string
		wantFetch, wantNotice int
	}{
		{"ten missed", State{LastSuccess: time.Unix(1, 0), NextAttempt: time.Unix(3601, 0)}, "v1", 1, 1},
		{"current", State{}, "", 1, 0},
		{"already notified", State{Notified: "v1"}, "v1", 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := save(filepath.Join(dir, "check.json"), tc.initial); err != nil {
				t.Fatal(err)
			}
			fetches, notices := 0, 0
			fetch := func(context.Context) (string, error) { fetches++; return tc.version, nil }
			notify := func(context.Context, string) error { notices++; return nil }
			now := time.Unix(36001, 0)
			for i := 0; i < 10; i++ {
				if _, err := Run(context.Background(), dir, now, fetch, notify); err != nil {
					t.Fatal(err)
				}
			}
			if fetches != tc.wantFetch || notices != tc.wantNotice {
				t.Fatalf("fetches=%d notices=%d", fetches, notices)
			}
			for i := 1; i <= 10; i++ {
				if _, err := Run(context.Background(), dir, now.Add(time.Duration(i)*time.Hour), fetch, notify); err != nil {
					t.Fatal(err)
				}
			}
			if notices != tc.wantNotice {
				t.Fatalf("duplicate notices: %d", notices)
			}
			info, err := os.Stat(filepath.Join(dir, "check.json"))
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0600 {
				t.Fatalf("mode %v", info.Mode())
			}
		})
	}
}

func TestConcurrentTriggers(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	var fetches, notices atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := Run(context.Background(), dir, time.Unix(40000, 0), func(context.Context) (string, error) {
				fetches.Add(1)
				time.Sleep(20 * time.Millisecond)
				return "v1", nil
			}, func(context.Context, string) error { notices.Add(1); return nil })
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if fetches.Load() != 1 || notices.Load() != 1 {
		t.Fatalf("fetches=%d notices=%d", fetches.Load(), notices.Load())
	}
}

func TestFailuresRetryWithoutBacklog(t *testing.T) {
	for _, kind := range []string{"network", "notification"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			now := time.Unix(40000, 0)
			fetches, notices := 0, 0
			fetch := func(context.Context) (string, error) {
				fetches++
				if kind == "network" && fetches == 1 {
					return "", errors.New("offline")
				}
				return "v1", nil
			}
			notify := func(context.Context, string) error {
				notices++
				if kind == "notification" && notices == 1 {
					return errors.New("no desktop")
				}
				return nil
			}
			state, err := Run(context.Background(), dir, now, fetch, notify)
			if err == nil || state.Notified != "" {
				t.Fatalf("state=%+v err=%v", state, err)
			}
			if _, err = Run(context.Background(), dir, now.Add(time.Minute), fetch, notify); err != nil {
				t.Fatal(err)
			}
			if fetches != 1 {
				t.Fatal("backoff ignored")
			}
			state, err = Run(context.Background(), dir, now.Add(5*time.Minute), fetch, notify)
			if err != nil || state.Notified != "v1" {
				t.Fatalf("state=%+v err=%v", state, err)
			}
			if kind == "network" && (fetches != 2 || notices != 1) || kind == "notification" && (fetches != 1 || notices != 2) {
				t.Fatalf("fetches=%d notices=%d", fetches, notices)
			}
		})
	}
}

func TestCrashReleasesLock(t *testing.T) {
	if dir := os.Getenv("T3_CHECK_LOCK_CHILD"); dir != "" {
		file, err := openPrivate(filepath.Join(dir, "check.lock"), syscall.O_RDWR|syscall.O_CREAT)
		if err != nil {
			os.Exit(2)
		}
		if syscall.Flock(int(file.Fd()), syscall.LOCK_EX) != nil {
			os.Exit(3)
		}
		os.Exit(0)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashReleasesLock$")
	cmd.Env = append(os.Environ(), "T3_CHECK_LOCK_CHILD="+dir)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, output)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := Run(ctx, dir, time.Now(), func(context.Context) (string, error) { return "", nil }, nil); err != nil {
		t.Fatal(err)
	}
}

func TestNotifierCrashRetainsPendingNotice(t *testing.T) {
	now := time.Unix(40000, 0)
	if dir := os.Getenv("T3_CHECK_NOTICE_CHILD"); dir != "" {
		_, err := Run(context.Background(), dir, now, func(context.Context) (string, error) { return "v1", nil }, func(context.Context, string) error { os.Exit(0); return nil })
		if err != nil {
			os.Exit(2)
		}
		os.Exit(3)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestNotifierCrashRetainsPendingNotice$")
	cmd.Env = append(os.Environ(), "T3_CHECK_NOTICE_CHILD="+dir)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, output)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	notices := 0
	state, err := Run(ctx, dir, now.Add(5*time.Minute), func(context.Context) (string, error) { t.Fatal("repeated fetch"); return "", nil }, func(context.Context, string) error { notices++; return nil })
	if err != nil || state.Notified != "v1" || notices != 1 {
		t.Fatalf("state=%+v notices=%d err=%v", state, notices, err)
	}
}

func TestRejectSymlinkState(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(target, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "check.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), dir, time.Now(), func(context.Context) (string, error) { t.Fatal("fetch called"); return "", nil }, nil); err == nil {
		t.Fatal("symlink accepted")
	}
	data, _ := os.ReadFile(target)
	if string(data) != "private" {
		t.Fatal("target changed")
	}
}

func TestBackwardClockOfflineHonorsRetry(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(40000, 0)
	if err := save(filepath.Join(dir, "check.json"), State{LastSuccess: now.Add(10 * time.Hour), NextAttempt: now.Add(11 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	fetch := func(context.Context) (string, error) { calls++; return "", errors.New("offline") }
	Run(context.Background(), dir, now, fetch, nil)
	Run(context.Background(), dir, now.Add(time.Minute), fetch, nil)
	if calls != 1 {
		t.Fatalf("clock caused %d calls", calls)
	}
}
