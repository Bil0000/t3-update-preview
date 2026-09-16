package host

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Bil0000/t3-update-preview/internal/config"
)

func TestRemoteRouteGuard(t *testing.T) {
	for _, test := range []struct {
		name, action, environment string
		status                    int
		wantCalls                 int
		wantError                 bool
	}{
		{"probe matches", "probe", "env-one", 200, 1, false},
		{"prepare matches", "prepare", "env-one", 200, 2, false},
		{"apply matches", "apply", "env-one", 200, 2, false},
		{"desktop without service", "apply", "env-one", 200, 2, false},
		{"missing runtime version", "apply", "env-one", 200, 1, true},
		{"target not activated", "apply", "env-one", 200, 2, true},
		{"route changed after apply", "apply", "env-one", 200, 2, true},
		{"wrong environment", "apply", "different", 200, 1, true},
		{"offline apply", "apply", "", 503, 1, true},
		{"offline cleanup", "cleanup", "", 503, 1, false},
		{"offline status", "status", "", 503, 1, false},
		{"offline rollback", "rollback", "", 503, 2, true},
		{"wrong rollback target", "rollback", "different", 200, 1, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			calls := filepath.Join(dir, "calls")
			args := filepath.Join(dir, "args")
			script := "#!/bin/sh\n/usr/bin/tee -a \"$NATIVE_TEST_CALLS\" >/dev/null\nprintf '%s\\n' \"$@\" > \"$NATIVE_TEST_ARGS\"\nprintf '%s' \"$NATIVE_TEST_RESPONSE\"\n"
			if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("NATIVE_TEST_CALLS", calls)
			t.Setenv("NATIVE_TEST_ARGS", args)
			response := Snapshot{MachineID: "pinned-host", EnvironmentID: "env-one", ServiceInstalled: true, ServiceVersion: "0.0.40", RuntimeVersion: "0.0.41-preview.20260916.1794"}
			if test.name == "desktop without service" {
				response.ServiceInstalled, response.ServiceVersion = false, ""
				response.DesktopRunning = true
			}
			if test.name == "missing runtime version" {
				response.RuntimeVersion = ""
			}
			encoded, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("NATIVE_TEST_RESPONSE", string(encoded))
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				environment := test.environment
				if test.name == "route changed after apply" && requests > 1 {
					environment = "different"
				}
				w.WriteHeader(test.status)
				_ = json.NewEncoder(w).Encode(map[string]string{"environmentId": environment, "serverVersion": "0.0.41-preview.20260916.1794"})
			}))
			defer server.Close()
			device := config.Device{ID: "fixture", Name: "Fixture", Kind: "relay", Host: "fixture-alias", URL: server.URL, MachineID: "pinned-host", BaseDir: "/tmp/literal ' $HOME ; value"}
			target := "0.0.41-preview.20260916.1794"
			if test.name == "target not activated" {
				target = "0.0.41-preview.20260916.1795"
			}
			_, err = (Client{}).Call(context.Background(), device, Request{Action: test.action, Target: target})
			if (err != nil) != test.wantError {
				t.Fatalf("error = %v, wantError %v", err, test.wantError)
			}
			if test.name == "offline rollback" && !strings.Contains(err.Error(), "host rollback completed; original T3 route remains unverified") {
				t.Fatalf("recovery outcome lost: %v", err)
			}
			data, err := os.ReadFile(calls)
			if err != nil || strings.Count(string(data), `"action":`) != test.wantCalls {
				t.Fatalf("unexpected worker calls %s: %v", data, err)
			}
			if test.wantCalls == 1 && test.wantError && strings.Contains(string(data), `"action":"`+test.action+`"`) {
				t.Fatalf("mutation ran before route check: %s", data)
			}
			data, err = os.ReadFile(args)
			if err != nil || !strings.HasPrefix(string(data), "-T\n-o\nBatchMode=yes\n-o\nStrictHostKeyChecking=yes\n-o\nConnectTimeout=10\n--\nfixture-alias\n") || strings.Contains(string(data), device.BaseDir) {
				t.Fatalf("unsafe SSH arguments: %v", err)
			}
		})
	}
}
