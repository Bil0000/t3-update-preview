package release

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fixtureClient(t *testing.T, respond func(*http.Request) (int, string)) *http.Client {
	t.Helper()
	return &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		status, body := respond(r)
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), ContentLength: -1, Request: r, Header: make(http.Header)}, nil
	})}
}

func asset(name, body string) Asset {
	return Asset{Name: name, URL: "https://github.com/pingdotgg/t3code/releases/download/v0.0.41-preview.20260916.1794/" + name, Digest: fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(body))), Size: int64(len(body))}
}

func TestCompare(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"0.0.1-preview.20260916.9", "v0.0.1-preview.20260916.10", -1},
		{"0.0.10-preview.1.1", "0.0.9-preview.99.99", 1},
		{"v0.0.1-preview.1.1", "0.0.1-preview.1.1", 0},
		{"0.0.1-preview.1.99999999999999999999999", "0.0.1-preview.1.9999999999999999999999", 1},
	} {
		got, err := Compare(tc.a, tc.b)
		if err != nil || got != tc.want {
			t.Fatalf("Compare(%s, %s) = %d, %v", tc.a, tc.b, got, err)
		}
	}
	for _, s := range []string{"", "0.0.1", "0.0.1-nightly.1.1", "0.0.1-preview.1", "0.0.1-preview.1.1+meta", "0.0.1-preview.01.1", "0.0.1-preview.1.-1", "0.0.1-preview.1.1\n", "V0.0.1-preview.1.1"} {
		if ValidVersion(s) {
			t.Errorf("accepted %q", s)
		}
		if _, err := Compare(s, "0.0.1-preview.1.1"); err == nil {
			t.Errorf("Compare accepted %q", s)
		}
	}
}

func TestLatest(t *testing.T) {
	calls := 0
	client := fixtureClient(t, func(r *http.Request) (int, string) {
		calls++
		if r.URL.Host != "api.github.com" || r.URL.Path != "/repos/pingdotgg/t3code/releases" || r.URL.Query().Get("page") != fmt.Sprint(calls) || r.URL.Query().Get("per_page") != "100" {
			t.Fatalf("unexpected request %s", r.URL)
		}
		rows := []Release{{Tag: "v0.0.1-preview.1.999", Draft: true}, {Tag: "v1.0.0"}, {Tag: "v0.0.1-nightly.2.1"}, {Tag: "v0.0.1-preview.1.9"}}
		if calls == 1 {
			for len(rows) < 100 {
				rows = append(rows, Release{Tag: "v1.0.0"})
			}
		} else {
			rows = append(rows, Release{Tag: "v0.0.1-preview.1.10"})
		}
		data, _ := json.Marshal(rows)
		return 200, string(data)
	})
	r, err := Latest(context.Background(), client)
	if err != nil || r.Version() != "0.0.1-preview.1.10" || calls != 2 {
		t.Fatalf("latest = %+v, %v; calls=%d", r, err, calls)
	}
}

func TestLatestFailures(t *testing.T) {
	rows, _ := json.Marshal(make([]Release, 100))
	for _, tc := range []struct {
		name   string
		status int
		body   string
		calls  int
	}{
		{"rate limit", 403, "{}", 1},
		{"unauthorized", 401, "{}", 1},
		{"no preview", 200, `[{"tag_name":"v1.0.0"}]`, 1},
		{"invalid JSON", 200, "[", 1},
		{"oversized JSON", 200, strings.Repeat(" ", maxJSON+1), 1},
		{"page exhaustion", 200, string(rows), 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			_, err := Latest(context.Background(), fixtureClient(t, func(*http.Request) (int, string) {
				calls++
				return tc.status, tc.body
			}))
			if err == nil || calls != tc.calls {
				t.Fatalf("error=%v calls=%d", err, calls)
			}
		})
	}
}

func TestRequiredOfficial1794Names(t *testing.T) {
	r := Release{Tag: "v0.0.41-preview.20260916.1794"}
	for _, name := range []string{
		"t3-0.0.41-preview.20260916.1794-darwin-arm64.tar.gz",
		"t3-0.0.41-preview.20260916.1794-linux-arm64.tar.gz",
		"t3-0.0.41-preview.20260916.1794-linux-x64.tar.gz",
		"T3-Code-0.0.41-preview.20260916.1794-arm64.dmg",
		"T3-Code-0.0.41-preview.20260916.1794-x64.dmg",
		"T3-Code-0.0.41-preview.20260916.1794-arm64.AppImage",
		"T3-Code-0.0.41-preview.20260916.1794-x86_64.AppImage",
	} {
		r.Assets = append(r.Assets, asset(name, "fixture"))
	}
	for _, tc := range []struct{ os, arch, suffix string }{
		{"darwin", "arm64", "arm64.dmg"},
		{"linux", "amd64", "x86_64.AppImage"},
		{"linux", "arm64", "arm64.AppImage"},
	} {
		assets, err := r.Required(tc.os, tc.arch, true)
		if err != nil || len(assets) != 2 || !strings.HasSuffix(assets[1].Name, tc.suffix) {
			t.Fatalf("%+v: %v, %v", tc, assets, err)
		}
	}
	for _, tc := range []struct{ os, arch string }{{"darwin", "x64"}, {"linux", "riscv64"}, {"windows", "amd64"}} {
		if _, err := r.Required(tc.os, tc.arch, true); err == nil {
			t.Errorf("accepted unsupported target %+v", tc)
		}
	}
	r.Assets[3].Digest = ""
	if _, err := r.Required("darwin", "arm64", true); err == nil {
		t.Fatal("desktop without digest accepted")
	}
	r.Assets[0].Digest = ""
	if _, err := r.Required("darwin", "arm64", false); err == nil {
		t.Fatal("CLI without digest or checksum accepted")
	}
	r.Assets = append(r.Assets, asset("SHA256SUMS", "fixture"))
	if _, err := r.Required("darwin", "arm64", false); err == nil {
		t.Fatal("unresolved CLI digest accepted with checksum asset present")
	}
	r.Assets = append(r.Assets, r.Assets[0])
	if _, err := r.Asset(r.Assets[0].Name); err == nil {
		t.Fatal("duplicate accepted")
	}
}

func checksumFixture() (Release, string) {
	a := asset("t3-0.0.41-preview.20260916.1794-linux-x64.tar.gz", "archive")
	body := strings.TrimPrefix(a.Digest, "sha256:") + "  " + a.Name + "\n"
	a.Digest = ""
	return Release{Tag: "v0.0.41-preview.20260916.1794", Assets: []Asset{a, asset("SHA256SUMS", body)}}, body
}

func TestLatestResolvesChecksumsForWorker(t *testing.T) {
	r, body := checksumFixture()
	metadata, _ := json.Marshal([]Release{r})
	calls := 0
	got, err := Latest(context.Background(), fixtureClient(t, func(req *http.Request) (int, string) {
		calls++
		switch req.URL.String() {
		case api + "?per_page=100&page=1":
			return 200, string(metadata)
		case r.Assets[1].URL:
			return 200, body
		default:
			t.Fatalf("unexpected request %s", req.URL)
			return 500, ""
		}
	}))
	if err != nil || calls != 2 {
		t.Fatalf("Latest error=%v calls=%d", err, calls)
	}
	assets, err := got.Required("linux", "amd64", false)
	if err != nil || len(assets) != 1 || assets[0].Digest != asset("fixture", "archive").Digest {
		t.Fatalf("worker metadata=%+v error=%v", assets, err)
	}
}

func TestChecksumFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Asset)
		body   func(string) string
	}{
		{"bad digest", func(a *Asset) { a.Digest = "sha256:" + strings.Repeat("0", 64) }, nil},
		{"invalid digest", func(a *Asset) { a.Digest = "sha256:bad" }, nil},
		{"missing digest", func(a *Asset) { a.Digest = "" }, nil},
		{"oversize metadata", func(a *Asset) { a.Size = maxJSON + 1 }, nil},
		{"zero size", func(a *Asset) { a.Size = 0 }, nil},
		{"truncated", nil, func(s string) string { return s[:len(s)-1] }},
		{"oversized response", nil, func(s string) string { return s + "extra" }},
		{"foreign host", func(a *Asset) { a.URL = "https://evil.example/SHA256SUMS" }, nil},
		{"foreign repo", func(a *Asset) { a.URL = strings.Replace(a.URL, "/pingdotgg/", "/evil/", 1) }, nil},
		{"wrong release", func(a *Asset) { a.URL = strings.Replace(a.URL, ".1794/", ".1793/", 1) }, nil},
		{"http", func(a *Asset) { a.URL = strings.Replace(a.URL, "https:", "http:", 1) }, nil},
		{"escaped path", func(a *Asset) { a.URL = strings.Replace(a.URL, "/SHA256SUMS", "/%53HA256SUMS", 1) }, nil},
		{"traversal", func(a *Asset) { a.URL = strings.Replace(a.URL, "/SHA256SUMS", "/../SHA256SUMS", 1) }, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, body := checksumFixture()
			if tc.change != nil {
				tc.change(&r.Assets[1])
			}
			if tc.body != nil {
				body = tc.body(body)
			}
			err := r.resolveChecksums(context.Background(), fixtureClient(t, func(*http.Request) (int, string) { return 200, body }))
			if err == nil || r.Assets[0].Digest != "" {
				t.Fatalf("accepted unsafe checksums: %v", err)
			}
		})
	}
}

func TestChecksumEntries(t *testing.T) {
	for _, kind := range []string{"missing", "duplicate", "malformed", "invalid digest"} {
		t.Run(kind, func(t *testing.T) {
			r, body := checksumFixture()
			switch kind {
			case "missing":
				body = strings.Replace(body, "linux-x64", "linux-arm64", 1)
			case "duplicate":
				body += body
			case "malformed":
				body = "bad checksum line\n"
			case "invalid digest":
				body = "bad  " + r.Assets[0].Name + "\n"
			}
			r.Assets[1] = asset("SHA256SUMS", body)
			if err := r.resolveChecksums(context.Background(), fixtureClient(t, func(*http.Request) (int, string) { return 200, body })); err == nil {
				t.Fatal("accepted invalid checksum entry")
			}
		})
	}
}

func TestChecksumRedirect(t *testing.T) {
	for _, host := range []string{"evil.example", "github.com.evil.example", "release-assets.githubusercontent.com"} {
		t.Run(host, func(t *testing.T) {
			r, body := checksumFixture()
			calls := 0
			client := &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
				calls++
				res := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), ContentLength: -1, Header: make(http.Header), Request: req}
				if calls == 1 {
					res.StatusCode = 302
					res.Header.Set("Location", "https://"+host+"/asset")
				}
				return res, nil
			})}
			err := r.resolveChecksums(context.Background(), client)
			if host == "release-assets.githubusercontent.com" {
				if err != nil || calls != 2 {
					t.Fatalf("%v calls=%d", err, calls)
				}
			} else if err == nil || calls != 1 {
				t.Fatalf("unsafe redirect followed: %v calls=%d", err, calls)
			}
		})
	}
}
