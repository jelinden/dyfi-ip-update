package main

import (
	"net/url"
	"testing"
)

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
