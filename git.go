package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var gitTimeout = 60 * time.Second

// git runs git in dir with no prompts and a time limit, and returns trimmed stdout.
func git(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=true", "SSH_ASKPASS=true")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("git %s: timed out after %s", args[0], gitTimeout)
	}
	if err != nil {
		msg := lastLine(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", args[0], redact(msg))
	}
	return strings.TrimSpace(out.String()), nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// redact drops credentials from any URL in a git message (https://user:token@host/...).
func redact(s string) string { return credRE.ReplaceAllString(s, "${1}***@") }

var credRE = regexp.MustCompile(`(https?://)[^/@\s'"]+@`)

// GitResolver answers {{git.<branch>}} from the repository holding the config. With Fetch it runs
// `git fetch origin <branch>` once per branch first, so the id is what the remote has now.
type GitResolver struct {
	Dir     string
	Fetch   bool
	Notes   []string // problems worth showing that did not stop the run
	fetched map[string]bool
	cache   map[string]refInfo
}

type refInfo struct {
	sha string
	at  time.Time
	err error
}

func (g *GitResolver) Env(name string) (string, bool) { return osEnv(name) }

func (g *GitResolver) Ref(name string) (string, time.Time, error) {
	if g.cache == nil {
		g.cache = map[string]refInfo{}
	}
	if r, ok := g.cache[name]; ok {
		return r.sha, r.at, r.err
	}
	r := g.resolve(name)
	g.cache[name] = r
	return r.sha, r.at, r.err
}

func (g *GitResolver) resolve(name string) refInfo {
	if _, err := git(g.Dir, "rev-parse", "--git-dir"); err != nil {
		return refInfo{err: fmt.Errorf("%s is not in a git repository", g.Dir)}
	}
	hasOrigin := false
	if _, err := git(g.Dir, "remote", "get-url", "origin"); err == nil {
		hasOrigin = true
	}
	if hasOrigin && g.Fetch {
		g.fetch(name)
	}
	ref := name
	if hasOrigin {
		ref = "origin/" + name
	}
	sha, err := git(g.Dir, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	if err != nil || sha == "" {
		return refInfo{err: fmt.Errorf("no branch %s", ref)}
	}
	ct, err := git(g.Dir, "log", "-1", "--format=%ct", sha)
	if err != nil {
		return refInfo{err: err}
	}
	sec, err := strconv.ParseInt(ct, 10, 64)
	if err != nil {
		return refInfo{err: fmt.Errorf("commit time %q: %v", ct, err)}
	}
	return refInfo{sha: sha, at: time.Unix(sec, 0)}
}

// fetch updates origin/<name>. A failure is a note, and the local copy of origin/<name> is used.
func (g *GitResolver) fetch(name string) {
	if g.fetched == nil {
		g.fetched = map[string]bool{}
	}
	if g.fetched[name] {
		return
	}
	g.fetched[name] = true
	if _, err := git(g.Dir, "fetch", "--quiet", "--no-tags", "origin", "--end-of-options", name); err != nil {
		g.Notes = append(g.Notes, fmt.Sprintf("could not fetch origin %s (%v); using the local origin/%s", name, err, name))
	}
}

// repoRoot is the top of the work tree holding dir, or "" outside git.
func repoRoot(dir string) string {
	out, err := git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	return out
}

// findConfig looks for livecheck.yaml in dir and its parents, stopping at the repository root.
func findConfig(dir string) (string, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		p := filepath.Join(dir, ConfigName)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", errors.New("no " + ConfigName + " here or in the folders above it, up to the repository root (use -f)")
}

// remoteKey turns a remote URL into host/owner/name, so clones and worktrees of one repository
// count once. Credentials and the scheme are dropped.
func remoteKey(u string) string {
	u = strings.TrimSpace(u)
	if u == "" {
		return ""
	}
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
		if at := strings.LastIndex(strings.SplitN(u, "/", 2)[0], "@"); at >= 0 {
			u = u[at+1:]
		}
	} else if at := strings.Index(u, "@"); at >= 0 && strings.Contains(u, ":") {
		// scp form: git@host:owner/name.git
		u = strings.Replace(u[at+1:], ":", "/", 1)
	}
	u = strings.TrimSuffix(strings.TrimSuffix(u, "/"), ".git")
	if i := strings.Index(u, "/"); i >= 0 {
		host := u[:i]
		if h, _, ok := strings.Cut(host, ":"); ok { // drop a port
			host = h
		}
		u = strings.ToLower(host) + u[i:]
	}
	return u
}

// defaultBranch is what origin/HEAD points at, else "main".
func defaultBranch(dir string) string {
	out, err := git(dir, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD")
	if err == nil {
		if b, ok := strings.CutPrefix(out, "origin/"); ok && b != "" {
			return b
		}
	}
	return "main"
}
