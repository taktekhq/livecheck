package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gitEnv isolates git from the user's config and fixes the author.
func gitEnv(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "Test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
}

func sh(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// commitAt writes files, commits them with the given commit time, and returns the commit id.
func commitAt(t *testing.T, dir string, at time.Time, files map[string]string) string {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	date := at.Format(time.RFC3339)
	env := []string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}
	sh(t, dir, env, "git", "add", "-A")
	sh(t, dir, env, "git", "commit", "-q", "--allow-empty", "-m", "c")
	return sh(t, dir, nil, "git", "rev-parse", "HEAD")
}

// originAndClone makes a bare origin with one commit on main and a clone of it. It returns
// (origin, clone, a scratch work tree that pushes to origin).
func originAndClone(t *testing.T, at time.Time, files map[string]string) (string, string, string) {
	return namedOrigin(t, "site", at, files)
}

func namedOrigin(t *testing.T, name string, at time.Time, files map[string]string) (string, string, string) {
	t.Helper()
	gitEnv(t)
	base := t.TempDir()
	origin := filepath.Join(base, name+".git")
	work := filepath.Join(base, "work")
	sh(t, base, nil, "git", "init", "-q", "--bare", "-b", "main", origin)
	sh(t, base, nil, "git", "init", "-q", "-b", "main", work)
	commitAt(t, work, at, files)
	sh(t, work, nil, "git", "remote", "add", "origin", origin)
	sh(t, work, nil, "git", "push", "-q", "origin", "main")
	clone := filepath.Join(base, "site")
	sh(t, base, nil, "git", "clone", "-q", origin, clone)
	return origin, clone, work
}
