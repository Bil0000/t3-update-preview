package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const api = "https://api.github.com/repos/pingdotgg/t3code/releases"
const maxJSON = 8 << 20
const maxDownload = 1 << 30

var versionPattern = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)-preview\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

type Asset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

type Release struct {
	Tag    string  `json:"tag_name"`
	Draft  bool    `json:"draft"`
	Assets []Asset `json:"assets"`
}

func (r Release) Version() string { return strings.TrimPrefix(r.Tag, "v") }

func ValidVersion(s string) bool { return versionPattern.MatchString(s) }

func Compare(a, b string) (int, error) {
	x, y := versionPattern.FindStringSubmatch(a), versionPattern.FindStringSubmatch(b)
	if x == nil || y == nil {
		return 0, errors.New("expected X.Y.Z-preview.NUM.NUM version")
	}
	for i := 1; i < len(x); i++ {
		if len(x[i]) < len(y[i]) {
			return -1, nil
		}
		if len(x[i]) > len(y[i]) {
			return 1, nil
		}
		if n := strings.Compare(x[i], y[i]); n != 0 {
			return n, nil
		}
	}
	return 0, nil
}

func httpClient(client *http.Client, redirect func(*url.URL) bool) *http.Client {
	c := http.Client{Timeout: 30 * time.Second}
	if client != nil {
		c = *client
	}
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 || !redirect(req.URL) {
			return errors.New("untrusted release redirect")
		}
		if client != nil && client.CheckRedirect != nil {
			return client.CheckRedirect(req, via)
		}
		return nil
	}
	return &c
}

func request(ctx context.Context, client *http.Client, address string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "t3-update-preview")
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		return nil, fmt.Errorf("GitHub release request failed: HTTP %d", res.StatusCode)
	}
	return res, nil
}

func readJSON(ctx context.Context, client *http.Client, address string, value any) error {
	res, err := request(ctx, httpClient(client, func(*url.URL) bool { return false }), address)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxJSON+1))
	if err != nil {
		return err
	}
	if len(data) > maxJSON {
		return errors.New("release metadata exceeds size limit")
	}
	return json.Unmarshal(data, value)
}

func Latest(ctx context.Context, client *http.Client) (Release, error) {
	var latest Release
	for page := 1; page <= 10; page++ {
		var releases []Release
		if err := readJSON(ctx, client, fmt.Sprintf("%s?per_page=100&page=%d", api, page), &releases); err != nil {
			return Release{}, err
		}
		for _, r := range releases {
			if r.Draft || !ValidVersion(r.Tag) {
				continue
			}
			if latest.Tag == "" {
				latest = r
			} else if cmp, _ := Compare(r.Tag, latest.Tag); cmp > 0 {
				latest = r
			}
		}
		if len(releases) < 100 {
			if latest.Tag == "" {
				return Release{}, errors.New("no published preview release found")
			}
			if err := latest.resolveChecksums(ctx, client); err != nil {
				return Release{}, err
			}
			return latest, nil
		}
	}
	return Release{}, errors.New("release pagination exceeded 10 pages; latest preview is uncertain")
}

func (r Release) Asset(name string) (Asset, error) {
	var found Asset
	for _, a := range r.Assets {
		if a.Name == name {
			if found.Name != "" {
				return Asset{}, fmt.Errorf("duplicate release asset %q", name)
			}
			found = a
		}
	}
	if found.Name == "" {
		return Asset{}, fmt.Errorf("release %s is missing %s", r.Tag, name)
	}
	return found, nil
}

func digest(a Asset) ([]byte, error) {
	if !strings.HasPrefix(a.Digest, "sha256:") {
		return nil, fmt.Errorf("asset %s has no SHA256 digest", a.Name)
	}
	hash, err := hex.DecodeString(strings.TrimPrefix(a.Digest, "sha256:"))
	if err != nil || len(hash) != sha256.Size {
		return nil, fmt.Errorf("asset %s has an invalid SHA256 digest", a.Name)
	}
	return hash, nil
}

func assetTag(a Asset) (string, error) {
	u, err := url.Parse(a.URL)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
		return "", errors.New("untrusted release asset URL")
	}
	parts := strings.Split(u.Path, "/")
	if len(parts) != 7 || strings.Join(parts[:5], "/") != "/pingdotgg/t3code/releases/download" || !ValidVersion(parts[5]) || parts[6] != a.Name || a.Name == "" || a.Name == "." || a.Name == ".." || strings.ContainsAny(a.Name, `/\\`) {
		return "", errors.New("release asset URL does not match official release and filename")
	}
	if a.Size <= 0 || a.Size > maxDownload {
		return "", errors.New("release asset size is missing or exceeds 1 GiB")
	}
	return parts[5], nil
}

func cliAsset(a Asset, version string) bool {
	for _, target := range []string{"darwin-arm64", "darwin-x64", "linux-arm64", "linux-x64"} {
		if a.Name == "t3-"+version+"-"+target+".tar.gz" {
			return true
		}
	}
	return false
}

func (r Release) Required(platform, arch string, desktop bool) ([]Asset, error) {
	if r.Draft || !ValidVersion(r.Tag) || (platform != "darwin" && platform != "linux") || (arch != "arm64" && arch != "x64" && arch != "amd64") {
		return nil, errors.New("unsupported preview release or platform")
	}
	if arch == "amd64" {
		arch = "x64"
	}
	names := []string{"t3-" + r.Version() + "-" + platform + "-" + arch + ".tar.gz"}
	if desktop {
		ext := ".dmg"
		if platform == "linux" {
			ext = ".AppImage"
			if arch == "x64" {
				arch = "x86_64"
			}
		}
		names = append(names, "T3-Code-"+r.Version()+"-"+arch+ext)
	}
	var assets []Asset
	for _, name := range names {
		a, err := r.Asset(name)
		if err != nil {
			return nil, err
		}
		tag, err := assetTag(a)
		if err != nil || tag != r.Tag {
			return nil, fmt.Errorf("asset %s has invalid release metadata", name)
		}
		if _, err := digest(a); err != nil {
			return nil, err
		}
		assets = append(assets, a)
	}
	return assets, nil
}

func (r *Release) resolveChecksums(ctx context.Context, client *http.Client) error {
	var missing []int
	for i, a := range r.Assets {
		if a.Digest == "" && cliAsset(a, r.Version()) {
			missing = append(missing, i)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	a, err := r.Asset("SHA256SUMS")
	if err != nil {
		return err
	}
	if tag, err := assetTag(a); err != nil || tag != r.Tag || a.Size > maxJSON {
		return errors.New("invalid SHA256SUMS metadata")
	}
	expected, err := digest(a)
	if err != nil {
		return err
	}
	res, err := request(ctx, httpClient(client, func(u *url.URL) bool {
		return u.Scheme == "https" && u.User == nil && (u.Host == "release-assets.githubusercontent.com" || u.Host == "objects.githubusercontent.com")
	}), a.URL)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.ContentLength >= 0 && res.ContentLength != a.Size {
		return errors.New("SHA256SUMS content length mismatch")
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, a.Size+1))
	if err != nil {
		return err
	}
	if int64(len(data)) != a.Size {
		return errors.New("SHA256SUMS truncated or exceeds declared size")
	}
	hash := sha256.Sum256(data)
	if !strings.EqualFold(hex.EncodeToString(hash[:]), hex.EncodeToString(expected)) {
		return errors.New("SHA256SUMS digest mismatch")
	}
	hashes := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return errors.New("malformed SHA256SUMS entry")
		}
		name := strings.TrimPrefix(fields[1], "*")
		entry := Asset{Name: name, Digest: "sha256:" + fields[0]}
		if _, err := digest(entry); err != nil {
			return err
		}
		if _, exists := hashes[name]; exists {
			return errors.New("duplicate SHA256SUMS entry")
		}
		hashes[name] = entry.Digest
	}
	for _, i := range missing {
		if hashes[r.Assets[i].Name] == "" {
			return fmt.Errorf("SHA256SUMS is missing %s", r.Assets[i].Name)
		}
		r.Assets[i].Digest = hashes[r.Assets[i].Name]
	}
	return nil
}
