package main

import (
	"strings"
	"testing"
	"time"
)

type fakeResolver struct {
	refs map[string]string
	at   time.Time
	env  map[string]string
}

func (f fakeResolver) Ref(name string) (string, time.Time, error) {
	sha, ok := f.refs[name]
	if !ok {
		return "", time.Time{}, errNoRef(name)
	}
	return sha, f.at, nil
}

func (f fakeResolver) Env(name string) (string, bool) { v, ok := f.env[name]; return v, ok }

type errNoRef string

func (e errNoRef) Error() string { return "no branch origin/" + string(e) }

const sha1 = "12c2e854f962b71255dcf8f693067e670a22f3e9"

func plans(t *testing.T, yml string, r Resolver) []Plan {
	t.Helper()
	c, err := ParseConfig([]byte(yml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ps, err := c.Plans(r)
	if err != nil {
		t.Fatalf("plans: %v", err)
	}
	return ps
}

func planErr(t *testing.T, yml string, r Resolver) string {
	t.Helper()
	c, err := ParseConfig([]byte(yml))
	if err != nil {
		return err.Error()
	}
	if _, err := c.Plans(r); err != nil {
		return err.Error()
	}
	t.Fatalf("no error for:\n%s", yml)
	return ""
}

func TestDefaults(t *testing.T) {
	ps := plans(t, `
checks:
  - name: home
    url: https://example.com/
  - name: post
    url: https://example.com/mcp
    body: '{}'
    contains: one
  - name: list
    url: https://example.com/
    contains: [a, b]
    follow_redirects: false
`, nil)
	h := ps[0]
	if h.Method != "GET" || h.Status != 200 || h.Timeout != defaultTimeout || h.Burst != 1 || !h.Follow {
		t.Errorf("defaults: %+v", h)
	}
	if ps[1].Method != "POST" {
		t.Errorf("a body should default to POST, got %s", ps[1].Method)
	}
	if len(ps[1].Contains) != 1 || ps[1].Contains[0].Value != "one" {
		t.Errorf("contains as a string: %+v", ps[1].Contains)
	}
	if len(ps[2].Contains) != 2 || ps[2].Follow {
		t.Errorf("contains as a list / follow_redirects: %+v", ps[2])
	}
}

func TestTemplates(t *testing.T) {
	r := fakeResolver{refs: map[string]string{"main": sha1, "release/v1": "abcdef1234567"}, at: time.Unix(100, 0),
		env: map[string]string{"TOKEN": "s3cret"}}
	ps := plans(t, `
checks:
  - name: build
    url: https://example.com/healthz?b={{ git.main.short }}
    json: {build: "{{git.main}}", nested: {list: ["{{git.release/v1}}", 3]}}
    headers: {Authorization: "Bearer {{env.TOKEN}}"}
    contains: "{{env.TOKEN}}"
    response_headers: {X-Build: "{{git.main.short}}"}
    grace: 2h
`, r)
	p := ps[0]
	if p.URL != "https://example.com/healthz?b=12c2e85" {
		t.Errorf("url: %s", p.URL)
	}
	if p.RawURL != "https://example.com/healthz?b={{ git.main.short }}" {
		t.Errorf("raw url: %s", p.RawURL)
	}
	m := p.JSON.(map[string]any)
	if m["build"] != sha1 {
		t.Errorf("json build: %v", m["build"])
	}
	list := m["nested"].(map[string]any)["list"].([]any)
	if list[0] != "abcdef1234567" || list[1] != 3 {
		t.Errorf("json nested: %v", list)
	}
	if p.Headers["Authorization"] != "Bearer s3cret" {
		t.Errorf("header: %v", p.Headers)
	}
	if p.Contains[0].Value != "s3cret" || p.Contains[0].Show != "{{env.TOKEN}}" {
		t.Errorf("an env value must be used but never shown: %+v", p.Contains[0])
	}
	if p.ResponseHeaders["X-Build"].Show != "12c2e85" {
		t.Errorf("a git value is shown: %+v", p.ResponseHeaders)
	}
	if len(p.Refs) != 2 || !p.Refs["main"].Equal(time.Unix(100, 0)) {
		t.Errorf("refs: %v", p.Refs)
	}
	if p.Grace != 2*time.Hour {
		t.Errorf("grace: %v", p.Grace)
	}
}

func TestConfigErrors(t *testing.T) {
	r := fakeResolver{refs: map[string]string{"main": sha1}, env: map[string]string{}}
	cases := map[string]struct{ yml, want string }{
		"empty":        {``, "empty"},
		"no checks":    {`checks: []`, "no checks"},
		"typo":         {"checks:\n  - name: a\n    url: https://x.y/\n    contain: z\n", "field contain not found"},
		"no name":      {"checks:\n  - url: https://x.y/\n", "name is required"},
		"no url":       {"checks:\n  - name: a\n", "url is required"},
		"bad scheme":   {"checks:\n  - name: a\n    url: ftp://x.y/\n", "http or https"},
		"twice":        {"checks:\n  - name: a\n    url: https://x.y/\n  - name: a\n    url: https://x.y/\n", "used twice"},
		"burst":        {"checks:\n  - name: a\n    url: https://x.y/\n    burst: 21\n", "between 1 and 20"},
		"timeout":      {"checks:\n  - name: a\n    url: https://x.y/\n    timeout: 5m\n", "ceiling"},
		"duration":     {"checks:\n  - name: a\n    url: https://x.y/\n    grace: soon\n", "not a duration"},
		"grace no git": {"checks:\n  - name: a\n    url: https://x.y/\n    grace: 2h\n", "grace needs"},
		"unknown tmpl": {"checks:\n  - name: a\n    url: https://x.y/{{date}}\n", "unknown template"},
		"no branch":    {"checks:\n  - name: a\n    url: https://x.y/\n    contains: '{{git.dev}}'\n", "no branch origin/dev"},
		"bad branch":   {"checks:\n  - name: a\n    url: https://x.y/\n    contains: '{{git.a..b}}'\n", "not a branch name"},
		"unset env":    {"checks:\n  - name: a\n    url: https://x.y/\n    contains: '{{env.NOPE}}'\n", "NOPE is not set"},
		"status":       {"checks:\n  - name: a\n    url: https://x.y/\n    status: 999\n", "not an HTTP status"},
		"empty substr": {"checks:\n  - name: a\n    url: https://x.y/\n    contains: ''\n", "empty string"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := planErr(t, c.yml, r)
			if !strings.Contains(got, c.want) {
				t.Errorf("error %q, want it to contain %q", got, c.want)
			}
		})
	}
}

func TestGitTemplateWithoutRepo(t *testing.T) {
	got := planErr(t, "checks:\n  - name: a\n    url: https://x.y/\n    contains: '{{git.main}}'\n", nil)
	if !strings.Contains(got, "no git repository") {
		t.Errorf("got %q", got)
	}
}
