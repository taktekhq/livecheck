package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// repo is one repository livecheck all will check.
type repo struct {
	name      string
	path      string
	hasOrigin bool
	branch    string
}

// discover finds git repositories under dir (up to depth levels down) that have a livecheck.yaml,
// either in the work tree or committed on origin/<branch>. Clones and worktrees of the same remote
// count once: the one whose folder is named like the repository wins, else the first by path.
func discover(dir string, depth int, branch string) ([]repo, error) {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("%s is not a folder", dir)
	}
	var paths []string
	var walk func(p string, level int)
	walk = func(p string, level int) {
		if isRepo(p) {
			paths = append(paths, p)
			return
		}
		if level >= depth {
			return
		}
		entries, err := os.ReadDir(p)
		if err != nil {
			return
		}
		for _, e := range entries {
			n := e.Name()
			if !e.IsDir() || strings.HasPrefix(n, ".") || n == "node_modules" || n == "vendor" {
				continue
			}
			walk(filepath.Join(p, n), level+1)
		}
	}
	walk(root, 0)

	groups := map[string][]repo{}
	var order []string
	for _, p := range paths {
		r := repo{path: p, name: filepath.Base(p)}
		key := ""
		if u, err := git(p, "remote", "get-url", "origin"); err == nil && u != "" {
			r.hasOrigin = true
			key = remoteKey(u)
			r.name = key[strings.LastIndex(key, "/")+1:]
		}
		if key == "" {
			common, err := git(p, "rev-parse", "--path-format=absolute", "--git-common-dir")
			if err != nil {
				continue
			}
			key = common
		}
		r.branch = branch
		if r.hasOrigin && r.branch == "" {
			r.branch = defaultBranch(p)
		}
		if !hasFile(filepath.Join(p, ConfigName)) && !(r.hasOrigin && committed(p, r.branch)) {
			continue
		}
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], r)
	}

	var out []repo
	for _, key := range order {
		g := groups[key]
		sort.Slice(g, func(i, j int) bool { return g[i].path < g[j].path })
		pick := g[0]
		for _, r := range g {
			if filepath.Base(r.path) == r.name {
				pick = r
				break
			}
		}
		out = append(out, pick)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].name != out[j].name {
			return out[i].name < out[j].name
		}
		return out[i].path < out[j].path
	})
	return out, nil
}

func isRepo(p string) bool { _, err := os.Stat(filepath.Join(p, ".git")); return err == nil }

func hasFile(p string) bool { st, err := os.Stat(p); return err == nil && !st.IsDir() }

func committed(dir, branch string) bool {
	_, err := git(dir, "cat-file", "-e", "origin/"+branch+":"+ConfigName)
	return err == nil
}

// check fetches the branch, reads the committed config and runs it. A repository without a remote
// uses its work tree file.
func (r repo) check(ctx context.Context, fetch bool) *FileReport {
	res := &GitResolver{Dir: r.path, Fetch: fetch}
	var data []byte
	source := ConfigName
	if r.hasOrigin {
		if fetch {
			res.fetch(r.branch)
		}
		source = "origin/" + r.branch + ":" + ConfigName
		out, err := gitBytes(r.path, "show", source)
		if err != nil {
			return &FileReport{Repo: r.name, Path: r.path, Source: source, Result: "skip",
				Error: fmt.Sprintf("%s is not on origin/%s yet", ConfigName, r.branch), Notes: res.Notes, Checks: []Result{}}
		}
		data = out
	} else {
		b, err := os.ReadFile(filepath.Join(r.path, ConfigName))
		if err != nil {
			return &FileReport{Repo: r.name, Path: r.path, Source: source, Result: Fail, Error: err.Error(), Checks: []Result{}}
		}
		data = b
	}
	rep := checkFile(ctx, data, res)
	rep.Repo, rep.Path, rep.Source = r.name, r.path, source
	return rep
}

func gitBytes(dir string, args ...string) ([]byte, error) {
	out, err := git(dir, args...)
	return []byte(out + "\n"), err
}
