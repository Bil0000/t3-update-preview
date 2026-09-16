package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Descriptor struct {
	EnvironmentID string `json:"environmentId"`
	ServerVersion string `json:"serverVersion"`
}

func Probe(ctx context.Context, endpoint, expectedVersion string) (Descriptor, error) {
	var descriptor Descriptor
	u, err := url.Parse(endpoint)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/" && u.Path != "/ws") {
		return descriptor, errors.New("native health needs a credential-free HTTP(S) or WS(S) origin or /ws URL")
	}
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	case "http", "https":
	default:
		return descriptor, errors.New("unsupported native health URL scheme")
	}
	u.Path = "/.well-known/t3/environment"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return descriptor, err
	}
	req.Header.Set("Accept", "application/json")
	client := http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := client.Do(req)
	if err != nil {
		return descriptor, fmt.Errorf("native descriptor request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return descriptor, fmt.Errorf("native descriptor returned HTTP %d", response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&descriptor); err != nil {
		return descriptor, fmt.Errorf("invalid native descriptor: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return descriptor, errors.New("native descriptor has trailing data")
	}
	if strings.TrimSpace(descriptor.EnvironmentID) == "" || strings.TrimSpace(descriptor.ServerVersion) == "" || strings.TrimSpace(descriptor.EnvironmentID) != descriptor.EnvironmentID || strings.TrimSpace(descriptor.ServerVersion) != descriptor.ServerVersion {
		return descriptor, errors.New("native descriptor lacks a valid environment ID or server version")
	}
	if expectedVersion != "" && descriptor.ServerVersion != expectedVersion {
		return descriptor, fmt.Errorf("native server version %s differs from expected %s", descriptor.ServerVersion, expectedVersion)
	}
	return descriptor, nil
}
