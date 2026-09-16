package discovery

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Bil0000/t3-update-preview/internal/config"
)

func Candidates(home string) ([]config.Device, error) {
	if !filepath.IsAbs(home) {
		return nil, errors.New("discovery needs an absolute home directory")
	}
	home, err := filepath.EvalSymlinks(home)
	if err != nil {
		return nil, err
	}
	devices := []config.Device{{ID: "local", Name: "local", Kind: "local"}}
	seen := make(map[string]bool)
	hosts := make(map[string]bool)
	var read func(string, bool) (bool, error)
	read = func(path string, conditional bool) (bool, error) {
		path, err := filepath.EvalSymlinks(path)
		if os.IsNotExist(err) {
			return conditional, nil
		}
		if err != nil {
			return conditional, err
		}
		rel, err := filepath.Rel(home, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return conditional, errors.New("SSH discovery includes must stay inside the supplied home directory")
		}
		if seen[path] {
			return conditional, nil
		}
		if len(seen) >= 128 {
			return conditional, errors.New("too many SSH include files")
		}
		seen[path] = true
		info, err := os.Stat(path)
		if err != nil {
			return conditional, err
		}
		if !info.Mode().IsRegular() || info.Size() > 1<<20 {
			return conditional, errors.New("SSH discovery input must be a regular file of at most 1 MiB")
		}
		f, err := os.Open(path)
		if err != nil {
			return conditional, err
		}
		defer f.Close()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			fields, err := split(scanner.Text())
			if err != nil {
				return conditional, err
			}
			if len(fields) == 0 {
				continue
			}
			switch strings.ToLower(fields[0]) {
			case "match":
				conditional = true
			case "host":
				conditional = false
				for _, host := range fields[1:] {
					if strings.ContainsAny(host, "*?![") || hosts[strings.ToLower(host)] {
						continue
					}
					id := "ssh-" + host
					if len(id) > 128 {
						id = fmt.Sprintf("ssh-%x", sha256.Sum256([]byte(host)))
					}
					device := config.Device{ID: id, Name: host, Kind: "ssh", Host: host}
					if config.Validate(config.Config{Version: 1, Devices: []config.Device{device}}) != nil {
						return conditional, errors.New("SSH configuration has an unsafe explicit Host alias")
					}
					hosts[strings.ToLower(host)] = true
					devices = append(devices, device)
				}
			case "include":
				if conditional {
					continue
				}
				for _, pattern := range fields[1:] {
					if strings.HasPrefix(pattern, "~/") {
						pattern = filepath.Join(home, pattern[2:])
					} else if strings.HasPrefix(pattern, "~") || strings.Contains(pattern, "%") {
						return conditional, errors.New("SSH discovery cannot resolve this Include path")
					} else if !filepath.IsAbs(pattern) {
						pattern = filepath.Join(home, ".ssh", pattern)
					}
					rel, err := filepath.Rel(home, pattern)
					if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
						return conditional, errors.New("SSH discovery Include patterns must stay inside the supplied home directory")
					}
					paths, err := filepath.Glob(pattern)
					if err != nil {
						return conditional, errors.New("invalid SSH Include pattern")
					}
					for _, included := range paths {
						conditional, err = read(included, conditional)
						if err != nil {
							return conditional, err
						}
					}
				}
			}
		}
		return conditional, scanner.Err()
	}
	_, err = read(filepath.Join(home, ".ssh", "config"), false)
	return devices, err
}

func split(line string) ([]string, error) {
	var fields []string
	var field strings.Builder
	var quote rune
	escaped := false
	for _, ch := range line {
		if escaped {
			field.WriteRune(ch)
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		if quote != 0 {
			if ch == quote {
				quote = 0
			} else {
				field.WriteRune(ch)
			}
			continue
		}
		if strings.ContainsRune("\"'", ch) {
			quote = ch
			continue
		}
		if ch == '#' {
			break
		}
		if ch == ' ' || ch == '\t' || ch == '\r' || ch == '=' && (len(fields) == 0 || len(fields) == 1 && field.Len() == 0) {
			if field.Len() > 0 {
				fields = append(fields, field.String())
				field.Reset()
			}
			continue
		}
		field.WriteRune(ch)
	}
	if quote != 0 || escaped {
		return nil, errors.New("malformed quoting in SSH configuration")
	}
	if field.Len() > 0 {
		fields = append(fields, field.String())
	}
	return fields, nil
}

func Import(reader io.Reader) ([]config.Device, error) {
	data, err := io.ReadAll(io.LimitReader(reader, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 1<<20 {
		return nil, errors.New("device import exceeds 1 MiB")
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var devices []config.Device
	if err := decoder.Decode(&devices); err != nil {
		return nil, errors.New("invalid device import JSON; expected an array of device records")
	}
	if devices == nil {
		return nil, errors.New("device import must be a JSON array")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("unexpected trailing device import data")
	}
	if err := config.Validate(config.Config{Version: 1, Devices: devices}); err != nil {
		return nil, err
	}
	for i := range devices {
		devices[i].MachineID = ""
	}
	return devices, nil
}
