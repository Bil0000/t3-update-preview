package check

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type State struct {
	LastSuccess time.Time `json:"last_success"`
	NextAttempt time.Time `json:"next_attempt"`
	Available   string    `json:"available"`
	Notified    string    `json:"notified"`
}

func Run(ctx context.Context, dir string, now time.Time, fetch func(context.Context) (string, error), notify func(context.Context, string) error) (State, error) {
	var state State
	if err := os.MkdirAll(dir, 0700); err != nil {
		return state, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return state, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		return state, fmt.Errorf("check directory must be private and owned by the current user: %s", dir)
	}
	lock, err := openPrivate(filepath.Join(dir, "check.lock"), syscall.O_RDWR|syscall.O_CREAT)
	if err != nil {
		return state, err
	}
	defer lock.Close()
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			return state, err
		}
		select {
		case <-ctx.Done():
			return state, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	path := filepath.Join(dir, "check.json")
	file, err := openPrivate(path, syscall.O_RDONLY)
	if err == nil {
		err = json.NewDecoder(file).Decode(&state)
		file.Close()
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return state, err
	}
	if err := ctx.Err(); err != nil {
		return state, err
	}
	if now.Before(state.NextAttempt) && state.NextAttempt.Sub(now) <= time.Hour {
		return state, nil
	}
	pending := state.Available != "" && state.Available != state.Notified
	if !pending || state.LastSuccess.IsZero() || now.Sub(state.LastSuccess) >= time.Hour || now.Before(state.LastSuccess) {
		version, fetchErr := fetch(ctx)
		if fetchErr != nil {
			state.NextAttempt = now.Add(5 * time.Minute)
			return state, errors.Join(fetchErr, save(path, state))
		}
		state.LastSuccess = now
		state.Available = version
		state.NextAttempt = now.Add(time.Hour)
		if version != "" && version != state.Notified && notify != nil {
			state.NextAttempt = now.Add(5 * time.Minute)
		}
		if err := save(path, state); err != nil {
			return state, err
		}
	}
	if state.Available != "" && state.Available != state.Notified && notify != nil {
		if err := notify(ctx, state.Available); err != nil {
			state.NextAttempt = now.Add(5 * time.Minute)
			return state, errors.Join(err, save(path, state))
		}
		state.Notified = state.Available
	}
	state.NextAttempt = state.LastSuccess.Add(time.Hour)
	return state, save(path, state)
}

func openPrivate(path string, flags int) (*os.File, error) {
	fd, err := syscall.Open(path, flags|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		file.Close()
		return nil, fmt.Errorf("check file must be private, regular, and owned by the current user: %s", path)
	}
	return file, nil
}

func save(path string, state State) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".check-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(file.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
