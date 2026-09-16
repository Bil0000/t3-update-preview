package native

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProbe(t *testing.T) {
	const valid = `{"environmentId":"env-one","serverVersion":"0.0.41-preview.20260916.1794","orchestrationProtocolVersion":2,"capabilities":{"serverSelfUpdate":"boot-service"}}`
	for _, test := range []struct {
		name, body, expected string
		status               int
		wantError            bool
	}{
		{"valid", valid, "0.0.41-preview.20260916.1794", 200, false},
		{"any version", valid, "", 200, false},
		{"wrong version", valid, "0.0.40", 200, true},
		{"unauthorized", valid, "", 401, true},
		{"missing identity", `{"serverVersion":"0.0.41"}`, "", 200, true},
		{"invalid identity", `{"environmentId":" env ","serverVersion":"0.0.41"}`, "", 200, true},
		{"html response", `<html>Sign in</html>`, "", 200, true},
		{"trailing JSON", valid + `{}`, "", 200, true},
		{"trailing garbage", valid + `x`, "", 200, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/.well-known/t3/environment" || r.Method != http.MethodGet || r.Header.Get("Authorization") != "" {
					t.Errorf("unexpected descriptor request: %s %s", r.Method, r.URL)
				}
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			descriptor, err := Probe(context.Background(), strings.Replace(server.URL, "http:", "ws:", 1)+"/ws", test.expected)
			if (err != nil) != test.wantError {
				t.Fatalf("Probe error = %v, wantError %v", err, test.wantError)
			}
			if err == nil && (descriptor.EnvironmentID != "env-one" || descriptor.ServerVersion != "0.0.41-preview.20260916.1794") {
				t.Fatalf("unexpected descriptor: %+v", descriptor)
			}
		})
	}
}

func TestProbeRejectsUnsafeURLsAndRedirects(t *testing.T) {
	for _, endpoint := range []string{"file:///etc/passwd", "https://user:secret@example.test", "https://example.test/?token=secret", "https://example.test/#secret", "https://example.test/api/connect", "https://example.test/%77s"} {
		if _, err := Probe(context.Background(), endpoint, ""); err == nil {
			t.Fatalf("accepted unsafe URL %s", endpoint)
		}
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Redirect(w, r, "/login", http.StatusFound)
	}))
	defer server.Close()
	if _, err := Probe(context.Background(), server.URL, ""); err == nil || requests != 1 {
		t.Fatalf("redirect was accepted or followed: err=%v requests=%d", err, requests)
	}
}
