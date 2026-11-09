package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGitResolverFetches(t *testing.T) {
	first := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	_, clone, work := originAndClone(t, first, map[string]string{"a.txt": "1"})
	second := first.Add(time.Hour)
	want := commitAt(t, work, second, map[string]string{"a.txt": "2"})
	sh(t, work, nil, "git", "push", "-q", "origin", "main")

	stale := &GitResolver{Dir: clone}
	sha, at, err := stale.Ref("main")
	if err != nil || sha == want || !at.Equal(first) {
		t.Errorf("without fetch the local origin/main is used: %s %s %v", sha, at, err)
	}
	fresh := &GitResolver{Dir: clone, Fetch: true}
	sha, at, err = fresh.Ref("main")
	if err != nil || sha != want || !at.Equal(second) {
		t.Errorf("with fetch: got %s %s %v, want %s %s", sha, at, err, want, second)
	}
	if len(fresh.Notes) != 0 {
		t.Errorf("notes: %v", fresh.Notes)
	}
	if _, _, err := fresh.Ref("nope"); err == nil || !strings.Contains(err.Error(), "no branch origin/nope") {
		t.Errorf("missing branch: %v", err)
	}
}

func TestGitResolverFetchFailureIsANote(t *testing.T) {
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	_, clone, _ := originAndClone(t, at, map[string]string{"a.txt": "1"})
	sh(t, clone, nil, "git", "remote", "set-url", "origin", "https://user:tok123@127.0.0.1:1/x.git")
	g := &GitResolver{Dir: clone, Fetch: true}
	sha, _, err := g.Ref("main")
	if err != nil || sha == "" {
		t.Fatalf("the local copy should still answer: %v", err)
	}
	if len(g.Notes) != 1 || !strings.Contains(g.Notes[0], "could not fetch origin main") {
		t.Fatalf("notes: %v", g.Notes)
	}
	if strings.Contains(g.Notes[0], "tok123") {
		t.Errorf("a note must not show credentials: %s", g.Notes[0])
	}
}

func TestGitResolverNoRemote(t *testing.T) {
	gitEnv(t)
	dir := t.TempDir()
	sh(t, dir, nil, "git", "init", "-q", "-b", "main")
	want := commitAt(t, dir, time.Unix(1700000000, 0), map[string]string{"x": "1"})
	sha, at, err := (&GitResolver{Dir: dir, Fetch: true}).Ref("main")
	if err != nil || sha != want || at.Unix() != 1700000000 {
		t.Errorf("%s %s %v", sha, at, err)
	}
	if _, _, err := (&GitResolver{Dir: t.TempDir()}).Ref("main"); err == nil || !strings.Contains(err.Error(), "not in a git repository") {
		t.Errorf("outside git: %v", err)
	}
}

func TestFindConfig(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "repo", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "repo", "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := findConfig(sub); err == nil {
		t.Error("no config: want an error")
	}
	// a config above the repository root is not used
	os.WriteFile(filepath.Join(root, ConfigName), []byte("x"), 0o644)
	if _, err := findConfig(sub); err == nil {
		t.Error("config outside the repository must not be found")
	}
	want := filepath.Join(root, "repo", ConfigName)
	os.WriteFile(want, []byte("x"), 0o644)
	if got, err := findConfig(sub); err != nil || got != want {
		t.Errorf("got %s %v, want %s", got, err, want)
	}
}

func TestRemoteKey(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:acme/app.git":                            "github.com/acme/app",
		"https://github.com/acme/app.git":                        "github.com/acme/app",
		"https://github.com/acme/app":                            "github.com/acme/app",
		"https://x-access-token:gho_abc@github.com/acme/app.git": "github.com/acme/app",
		"ssh://git@GitHub.com:22/acme/app.git/":                  "github.com/acme/app",
		"/srv/git/app.git":                                       "/srv/git/app",
	} {
		if got := remoteKey(in); got != want {
			t.Errorf("remoteKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRedact(t *testing.T) {
	in := "fatal: unable to access 'https://x-access-token:gho_abc@github.com/a/b.git/': error"
	got := redact(in)
	if strings.Contains(got, "gho_abc") || !strings.Contains(got, "https://***@github.com/a/b.git/") {
		t.Errorf("redact: %s", got)
	}
}
