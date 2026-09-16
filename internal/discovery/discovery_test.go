package discovery

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCandidatesIncludesAndWildcards(t *testing.T) {
	home := t.TempDir()
	files := map[string]string{
		"config":           "Host first *.example !denied\nInclude = \"conf.d/*.conf\"\nMatch exec \"touch never-run\"\nInclude conditional.conf\nHost last\nInclude missing.conf\n",
		"conf.d/a.conf":    "Host=second first\nInclude config\nInclude ~/extra.conf\n",
		"conditional.conf": "Host hidden\n",
	}
	for name, body := range files {
		path := filepath.Join(home, ".ssh", name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "extra.conf"), []byte("Host =third # comment\n"), 0600); err != nil {
		t.Fatal(err)
	}
	devices, err := Candidates(home)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, d := range devices {
		ids = append(ids, d.ID)
	}
	if !reflect.DeepEqual(ids, []string{"local", "ssh-first", "ssh-second", "ssh-third", "ssh-last"}) {
		t.Fatalf("wrong candidates: %v", ids)
	}
	again, err := Candidates(home)
	if err != nil || !reflect.DeepEqual(devices, again) {
		t.Fatalf("unstable discovery: %v", err)
	}
}

func TestCandidatesUnsafeIncludesAndAliases(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".ssh"), 0700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "hosts"), []byte("Host outside\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "hosts"), filepath.Join(home, ".ssh", "link")); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		"Include " + filepath.Join(outside, "*") + "\n",
		"Include link\n",
		"Host -option\n",
		"Host \"unterminated\n",
		"Host host;bad\n",
	} {
		if err := os.WriteFile(filepath.Join(home, ".ssh", "config"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Candidates(home); err == nil {
			t.Fatalf("unsafe discovery accepted: %q", body)
		}
	}
}

func TestCandidatesWithoutSSHAndImport(t *testing.T) {
	devices, err := Candidates(t.TempDir())
	if err != nil || len(devices) != 1 || devices[0].ID != "local" {
		t.Fatalf("local candidate: %v %v", devices, err)
	}
	devices, err = Import(strings.NewReader(`[{"id":"server","name":"server","kind":"relay","url":"wss://example.test","host":"recovery","machine_id":"unverified"}]`))
	if err != nil || len(devices) != 1 || devices[0].Host != "recovery" || devices[0].MachineID != "" {
		t.Fatalf("import: %v %v", devices, err)
	}
	for _, body := range []string{
		`[{"id":"server","name":"server","kind":"ssh","host":"server","token":"secret"}]`,
		`[{"id":"server","name":"server","kind":"direct","url":"https://example.test?token=secret"}]`,
		`[] {}`,
		`null`,
		`{"devices":[]}`,
		strings.Repeat(" ", (1<<20)+1),
	} {
		if _, err := Import(strings.NewReader(body)); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("invalid import accepted or leaked: %v", err)
		}
	}
}
