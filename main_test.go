package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func cli(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()
	if dir != "" {
		old, _ := os.Getwd()
		if err := os.Chdir(dir); err != nil {
			t.Fatal(err)
		}
		defer os.Chdir(old)
	}
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func writeConfig(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, ConfigName)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExitCodes(t *testing.T) {
	u := server(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"status":"ok"}`) })
	dir := t.TempDir()

	writeConfig(t, dir, "checks:\n  - name: up\n    url: "+u+"\n    json: {status: ok}\n")
	if code, out, _ := cli(t, dir); code != exitPass || !strings.Contains(out, "PASS  up") || !strings.Contains(out, "1 passed, 0 failed, 0 warned") {
		t.Errorf("pass: %d %q", code, out)
	}

	bad := filepath.Join(t.TempDir(), "wrong.yaml")
	os.WriteFile(bad, []byte("checks:\n  - name: wrong\n    url: "+u+"\n    json: {status: down}\n"), 0o644)
	code, out, _ := cli(t, "", "-f", bad)
	if code != exitFail || !strings.Contains(out, `FAIL  wrong  200`) || !strings.Contains(out, `json $.status is "ok", want "down"`) {
		t.Errorf("fail: %d %q", code, out)
	}

	broken := filepath.Join(t.TempDir(), "broken.yaml")
	os.WriteFile(broken, []byte("checks:\n  - name: x\n    urll: "+u+"\n"), 0o644)
	if code, _, errs := cli(t, "", "-f", broken); code != exitUsage || !strings.Contains(errs, "field urll not found") {
		t.Errorf("config error: %d %q", code, errs)
	}
	if code, _, errs := cli(t, t.TempDir()); code != exitUsage || !strings.Contains(errs, "no livecheck.yaml") {
		t.Errorf("no config: %d %q", code, errs)
	}
	if code, _, _ := cli(t, "", "-nope"); code != exitUsage {
		t.Errorf("bad flag: %d", code)
	}
	if code, out, _ := cli(t, "", "version"); code != 0 || out != "livecheck "+version+"\n" {
		t.Errorf("version: %d %q", code, out)
	}
}

func TestDeployedBuildWithGrace(t *testing.T) {
	build := "unknown"
	u := server(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, `{"build":%q}`, build) })
	cfg := "checks:\n  - name: deployed build is main\n    url: " + u + "/healthz\n    json: {build: '{{git.main}}'}\n    grace: 2h\n"

	// merged 10 minutes ago, not deployed yet: warn, exit 3
	_, clone, work := originAndClone(t, time.Now().Add(-10*time.Minute), map[string]string{ConfigName: cfg})
	code, out, _ := cli(t, clone)
	if code != exitWarn || !strings.Contains(out, "WARN  deployed build is main") || !strings.Contains(out, "within grace 2h") {
		t.Errorf("young merge: %d %q", code, out)
	}

	// the deploy lands with a short id: pass
	head := sh(t, clone, nil, "git", "rev-parse", "origin/main")
	build = head[:7]
	if code, out, _ := cli(t, clone); code != exitPass {
		t.Errorf("deployed: %d %q", code, out)
	}

	// a merge 3 hours ago that never deployed: fail, and livecheck fetched it although the clone is stale
	commitAt(t, work, time.Now().Add(-3*time.Hour), map[string]string{"b.txt": "x"})
	sh(t, work, nil, "git", "push", "-q", "origin", "main")
	code, out, _ = cli(t, clone)
	if code != exitFail || !strings.Contains(out, "FAIL  deployed build is main") || strings.Contains(out, "grace") {
		t.Errorf("merged not deployed: %d %q", code, out)
	}
	// -no-fetch reads the local origin/main, which the run above already updated
	if code, _, _ := cli(t, clone, "-no-fetch"); code != exitFail {
		t.Errorf("-no-fetch after a fetch: %d", code)
	}
}

func TestJSONAndOutFile(t *testing.T) {
	u := server(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "hello") })
	dir := t.TempDir()
	writeConfig(t, dir, "checks:\n  - name: a\n    url: "+u+"\n    contains: hello\n  - name: b\n    url: "+u+"\n    contains: bye\n    burst: 2\n")
	outFile := filepath.Join(t.TempDir(), "state", "last.json")
	code, out, _ := cli(t, dir, "-json", "-o", outFile)
	if code != exitFail {
		t.Errorf("code %d", code)
	}
	var rep FileReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if rep.Passed != 1 || rep.Failed != 1 || rep.Result != Fail || len(rep.Checks) != 2 || rep.Checks[1].Burst != 2 {
		t.Errorf("report: %+v", rep)
	}
	if rep.Checks[1].Reason != `2 of 2 failed, first: body lacks "bye": hello` {
		t.Errorf("reason: %q", rep.Checks[1].Reason)
	}
	saved, err := os.ReadFile(outFile)
	if err != nil || string(saved) != out {
		t.Errorf("-o file: %v %q", err, saved)
	}
}

func TestAll(t *testing.T) {
	gitEnv(t)
	u := server(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "up") })
	root := t.TempDir()
	good := "checks:\n  - name: home\n    url: " + u + "/\n    contains: up\n"
	failing := "checks:\n  - name: home\n    url: " + u + "/\n    contains: down\n"

	// site: config committed on main; cloned twice under root (the clones count once)
	_, _, siteWork := namedOrigin(t, "site", time.Now().Add(-time.Hour), map[string]string{ConfigName: good})
	siteOrigin := strings.TrimSpace(sh(t, siteWork, nil, "git", "remote", "get-url", "origin"))
	sh(t, root, nil, "git", "clone", "-q", siteOrigin, filepath.Join(root, "site"))
	sh(t, root, nil, "git", "clone", "-q", siteOrigin, filepath.Join(root, "site-wt"))
	// the canonical clone sits on a feature branch without the config: main's copy is still used
	sh(t, filepath.Join(root, "site"), nil, "git", "checkout", "-q", "-b", "feature")
	os.Remove(filepath.Join(root, "site", ConfigName))

	// broken: config committed on main, fails
	_, _, brokenWork := namedOrigin(t, "broken", time.Now().Add(-time.Hour), map[string]string{ConfigName: failing})
	brokenOrigin := strings.TrimSpace(sh(t, brokenWork, nil, "git", "remote", "get-url", "origin"))
	sh(t, root, nil, "git", "clone", "-q", brokenOrigin, filepath.Join(root, "group", "broken"))

	// draft: config only in the work tree of a branch, not on origin/main: skipped
	_, _, draftWork := namedOrigin(t, "draft", time.Now().Add(-time.Hour), map[string]string{"README": "x"})
	draftOrigin := strings.TrimSpace(sh(t, draftWork, nil, "git", "remote", "get-url", "origin"))
	sh(t, root, nil, "git", "clone", "-q", draftOrigin, filepath.Join(root, "draft"))
	writeConfig(t, filepath.Join(root, "draft"), good)

	// local: no remote, work tree config
	local := filepath.Join(root, "local")
	sh(t, root, nil, "git", "init", "-q", "-b", "main", local)
	writeConfig(t, local, good)

	// plain: a repository without any config is ignored
	sh(t, root, nil, "git", "init", "-q", "-b", "main", filepath.Join(root, "plain"))

	outFile := filepath.Join(t.TempDir(), "last.txt")
	code, out, errs := cli(t, "", "all", "-o", outFile, root)
	if code != exitFail {
		t.Errorf("code %d, stderr %q", code, errs)
	}
	for _, want := range []string{
		"FAIL  broken",
		`      FAIL  home  200`,
		`body lacks "down": up`,
		"SKIP  draft   livecheck.yaml is not on origin/main yet",
		"PASS  local   1/1 passed",
		"PASS  site    1/1 passed",
		"livecheck all: 4 repos, 2 passed, 1 failed, 0 warned, 1 skipped",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "site") != 1 || strings.Contains(out, "plain") {
		t.Errorf("clones must count once and repos without config are ignored:\n%s", out)
	}
	if saved, _ := os.ReadFile(outFile); string(saved) != out {
		t.Errorf("-o file differs")
	}

	// JSON, and a depth that does not reach group/broken
	code, out, _ = cli(t, "", "all", "-json", "-depth", "1", root)
	var all AllReport
	if err := json.Unmarshal([]byte(out), &all); err != nil {
		t.Fatalf("json: %v", err)
	}
	if code != exitPass || all.Failed != 0 || all.Passed != 2 || all.Skipped != 1 {
		t.Errorf("depth 1: %d %+v", code, all)
	}
	for _, r := range all.Repos {
		if r.Repo == "site" && (r.Source != "origin/main:livecheck.yaml" || filepath.Base(r.Path) != "site") {
			t.Errorf("source: %s", r.Source)
		}
	}
}

func TestAllEmpty(t *testing.T) {
	gitEnv(t)
	code, out, _ := cli(t, "", "all", t.TempDir())
	if code != exitPass || !strings.Contains(out, "has a livecheck.yaml") {
		t.Errorf("%d %q", code, out)
	}
}
