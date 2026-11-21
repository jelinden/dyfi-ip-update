package main

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dyfi.json")
	content := `{"domains": [" Jelinden.fi ", "jelinden.fi", "", "uutispuro.fi"]}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"jelinden.fi", "uutispuro.fi"} // trimmed, lowercased, deduped
	if len(cfg.Domains) != len(want) {
		t.Fatalf("got %v, want %v", cfg.Domains, want)
	}
	for i := range want {
		if cfg.Domains[i] != want[i] {
			t.Errorf("domains[%d] = %q, want %q", i, cfg.Domains[i], want[i])
		}
	}
}

func TestLoadConfigMissingFileUsesDefaults(t *testing.T) {
	cfg, err := loadConfig(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Domains) != len(defaultDomains) {
		t.Errorf("got %d domains, want %d defaults", len(cfg.Domains), len(defaultDomains))
	}
}

func TestLoadConfigErrors(t *testing.T) {
	dir := t.TempDir()

	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte("{not json"), 0o600)
	if _, err := loadConfig(bad); err == nil {
		t.Error("expected parse error")
	}

	empty := filepath.Join(dir, "empty.json")
	b, _ := json.Marshal(Config{Domains: []string{" ", ""}})
	os.WriteFile(empty, b, 0o600)
	if _, err := loadConfig(empty); err == nil {
		t.Error("expected error for empty domains list")
	}
}

func TestParseAddForm(t *testing.T) {
	page := `<html><body>
	<form method="post" action="/?c=changepw"><input name="x" /></form>
	<form method="post" action="/nic/add">
		<input name="c" type="hidden" value="add" />
		<input name="hostname" type="text" value="" size="30" />
		<input type="submit" name="submit" value="Add" />
	</form>
	</body></html>`

	action, fields, err := parseAddForm(page)
	if err != nil {
		t.Fatalf("parseAddForm: %v", err)
	}
	if want := "https://www.dy.fi/nic/add"; action != want {
		t.Errorf("action = %q, want %q", action, want)
	}
	if got := fields.Get("c"); got != "add" {
		t.Errorf("hidden field c = %q, want %q", got, "add")
	}
	if got := fields.Get("hostname"); got != "" {
		t.Errorf("hostname = %q, want empty", got)
	}
}

func TestParseAddFormRelativeAndMissing(t *testing.T) {
	// action="/?" style (like dy.fi's login form) must resolve correctly.
	page := `<form method="post" action="/?"><input name="hostname" value="" /></form>`
	action, _, err := parseAddForm(page)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(action)
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "www.dy.fi" || u.Path != "/" {
		t.Errorf("resolved to %q", action)
	}

	if _, _, err := parseAddForm("<html>no forms here</html>"); err == nil {
		t.Error("expected error when no add form present")
	}
}

func TestEnqueueDedupesPerHost(t *testing.T) {
	// use a private channel-backed scenario on the global queue
	dequeue("dedupe-test.fi") // ensure clean state
	if !enqueue(job{host: "dedupe-test.fi", reason: "first"}) {
		t.Fatal("first enqueue should succeed")
	}
	if enqueue(job{host: "dedupe-test.fi", reason: "duplicate"}) {
		t.Error("second enqueue for same host should be coalesced")
	}
	dequeue("dedupe-test.fi")
	if !enqueue(job{host: "dedupe-test.fi", reason: "after dequeue"}) {
		t.Error("enqueue after dequeue should succeed")
	}
	dequeue("dedupe-test.fi")
}

func TestLoggedInDetection(t *testing.T) {
	loggedOut := `<a href="/?c=forgot">Unohtuiko salasana?</a>
		<input type="password" name="password" size="19" />`
	if loggedIn(loggedOut) {
		t.Error("page with login form must not be considered logged in")
	}

	for _, page := range []string{
		`<a href="/?c=logout">kirjaudu ulos</a>`,
		`<a href="/?c=logout">log out</a>`,
		`<a href="/nic/changepw">change password</a>`,
	} {
		if !loggedIn(page) {
			t.Errorf("expected logged-in detection for: %q", page)
		}
	}
}

func TestUpdateResultString(t *testing.T) {
	if resultNoHost.String() != "no-host" {
		t.Errorf("got %q", resultNoHost.String())
	}
}

func TestClassifyUpdateResponse(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   updateResult
	}{
		{200, "good 86.60.133.218", resultOK},
		{200, "nochg\n", resultOK},
		{401, "badauth", resultAuthFail},
		{200, "nohost", resultNoHost},
		{200, "notfqdn", resultNoHost},
		{200, "dnserr", resultTransient},
		{200, "abuse", resultTransient},
		{200, "badip 1.2.3.4", resultTransient},
		{503, "", resultTransient},
		{200, "something weird", resultTransient},
	}
	for _, c := range cases {
		if got, _ := classifyUpdateResponse(c.status, c.body); got != c.want {
			t.Errorf("classify(%d, %q) = %v, want %v", c.status, c.body, got, c.want)
		}
	}
}

func TestJitterForDeterministicAndBounded(t *testing.T) {
	a, b := jitterFor("jelinden.fi"), jitterFor("jelinden.fi")
	if a != b {
		t.Errorf("jitter not deterministic: %v vs %v", a, b)
	}
	if a < 0 || a >= refreshJitter {
		t.Errorf("jitter out of bounds: %v", a)
	}
}
