# livecheck

Check that production shows the change, not just that the change was merged.

A merged pull request is not a deployed one. A deploy that "succeeded" can still serve the old build, an empty table or a broken endpoint. People and AI agents both report work as done at the merge, and the gap shows up days later. `livecheck` closes it with one file per repository that says what production must show, and one command that checks it:

```console
$ livecheck
PASS  home page               200    412ms
PASS  search API              200    388ms
WARN  deployed build is main  200    176ms  json $.build is "3735aaf", want "12c2e854f962b71255dcf8f693067e670a22f3e9" (main moved 14m ago, within grace 2h: may still be deploying)
PASS  MCP get_article x6      200    402ms
livecheck: 3 passed, 0 failed, 1 warned
```

Make it the definition of done: run it after every deploy (by hand, in CI, or as the done-check of an AI agent), and nightly for every repository, so a merge that never reached production fails loudly.

One binary, one dependency (YAML parsing).

## Install

```sh
go install github.com/taktekhq/livecheck@latest
```

## livecheck.yaml

Put it at the repository root, next to the code it checks.

```yaml
checks:
  - name: home page
    url: https://example.com/
    contains: "Example"
    max_ms: 2000

  - name: deployed build is main
    url: https://example.com/healthz          # answers {"status":"ok","build":"<commit id>"}
    json: {build: "{{git.main}}"}
    grace: 2h                                 # a merge younger than this may still be deploying

  - name: MCP get_article
    url: https://example.com/mcp
    method: POST
    headers: {Content-Type: application/json, Accept: "application/json, text/event-stream"}
    body: '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_article","arguments":{"id":1}}}'
    json: {result: {isError: false}}
    contains: text
    burst: 6                                  # six requests at once; every one must pass
    max_ms: 1500
```

| Field | Default | Meaning |
|---|---|---|
| `name` | required | Shown in the output. Unique in the file. |
| `url` | required | `http` or `https`. |
| `method` | `GET`, or `POST` with a `body` | |
| `headers` | none | Request headers. `User-Agent` is `livecheck/<version>` unless you set it, so you can leave the checks out of your analytics. |
| `body` | none | Request body. |
| `status` | `200` | The status every response must have. |
| `contains` | none | A string, or a list; each must appear in the body. |
| `not_contains` | none | A string, or a list; none may appear. |
| `json` | none | The body must be JSON (or an event stream, whose last `data:` line is used) that contains this. Objects match when every key you list matches; other keys are ignored. Arrays match item by item. Strings match exactly, except that two commit ids match when the shorter is a prefix of the longer, so `12c2e85` matches the full id. |
| `response_headers` | none | Response headers that must have these values (same string rule as `json`). |
| `max_ms` | none | Every response must take at most this long, body included. |
| `timeout` | `10s` | Per request. Ceiling `2m`. |
| `burst` | `1` | Send this many requests at the same moment; every one must pass. Up to 20. Useful to catch rate limits and connection pool limits that a single request never hits. |
| `grace` | none | When this check fails but the branch in its `{{git.<branch>}}` template got its last commit less than `grace` ago, the result is a warning, not a failure: the deploy may still be running. |
| `follow_redirects` | `true` | Set `false` to check a redirect itself (`status: 301`). |

Unknown fields are errors, so a typo cannot silently turn a check off.

### Templates

Any string can use:

| Template | Value |
|---|---|
| `{{git.main}}` | The full commit id of `origin/main` in the repository holding the file. livecheck runs `git fetch origin main` first, so it is what the remote has now, not what your clone last saw. Any branch works: `{{git.release}}`. |
| `{{git.main.short}}` | The first 7 characters, for pages that show a short id. |
| `{{env.NAME}}` | An environment variable, for a token a check needs. Its value is sent but never printed: failure messages show `{{env.NAME}}` instead. |

If the fetch fails (offline, no access), livecheck uses the local `origin/main` and prints a note.

### Exposing the build

The "deployed build is main" check needs production to tell which commit it runs. Bake the commit id in at build time and serve it:

```go
var BuildID = "unknown" // go build -ldflags "-X main.BuildID=$(git rev-parse HEAD)"

http.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, `{"status":"ok","build":%q}`, BuildID)
})
```

A response header (`response_headers: {X-Build: "{{git.main}}"}`) or a `<meta name="build">` tag (`contains: '<meta name="build" content="{{git.main}}"'`) works as well.

## Commands

```sh
livecheck [-f FILE] [-json] [-o FILE] [-no-fetch]
livecheck all [-json] [-o FILE] [-depth 2] [-ref BRANCH] [-no-fetch] [-v] [DIR]
livecheck version
```

`livecheck` runs `./livecheck.yaml`, or the nearest one in a parent folder up to the repository root. Checks run one after another; a burst runs its requests together. Output is one line per check: the result, the HTTP status, the slowest response time, and for a failure the reason with at most 200 bytes of the part of the body that failed.

`livecheck all DIR` finds every git repository under `DIR` (up to `-depth` levels) and runs the `livecheck.yaml` **committed on its main branch** (origin/HEAD, or `-ref`), after a fetch. That way a clone that sits on a feature branch is still checked against what was merged, and a config that is only on a branch is reported as `SKIP` instead of failing. Clones and worktrees of the same remote count once. A repository without a remote uses its work tree file. Output is one line per repository, plus the checks that did not pass:

```console
$ livecheck all ~/src
livecheck 0.1.0: /home/me/src, 2026-11-09 05:15 UTC
PASS  docs   3/3 passed
FAIL  shop   2/3 passed
      FAIL  deployed build is main  200    121ms  json $.build is "9f1c2ab", want "4e5d6f70c1b2a3948576d1e2f3a4b5c6d7e8f901"
livecheck all: 2 repos, 1 passed, 1 failed, 0 warned, 0 skipped
```

`-o FILE` also writes the report to a file (atomically), for a nightly job. `-json` prints the same as JSON.

Remote URLs are never printed, and credentials in git error messages are masked.

### Exit status

| Code | Meaning |
|---|---|
| 0 | Every check passed. |
| 1 | At least one check failed (in `all`: a repository failed, or its file is invalid). |
| 2 | Bad flags or an invalid config file. |
| 3 | Nothing failed, but a check is within its grace: the deploy may still be running. Try again later. |

## Use it as a done-check

For an AI agent or a script that ships changes: "done" means `livecheck` exits 0 in the repository after the deploy. Paste its output in the report. An exit code of 3 means not yet; wait and run it again.

With a background runner that decides a task's status by a command's exit code, use `livecheck` as that command. The check needs network access to the sites it checks.

## Nightly

A systemd user timer:

```ini
# ~/.config/systemd/user/livecheck.service
[Service]
Type=oneshot
ExecStart=%h/go/bin/livecheck all -o %h/.local/state/livecheck/last.txt %h/src

# ~/.config/systemd/user/livecheck.timer
[Timer]
OnCalendar=*-*-* 05:15:00
[Install]
WantedBy=timers.target
```

```sh
systemctl --user daemon-reload && systemctl --user enable --now livecheck.timer
```

A "deployed build is main" check that still fails after its grace is the merged-but-never-deployed case.

## Be gentle with production

Checks are for reading. Use `GET`, or `POST` only to endpoints that change nothing (search, read-only MCP tools). Keep `burst` small, and remember that a nightly job and a few people running it by hand all come from the same addresses.

## Tests

```sh
go test ./...
```

They run against local test servers and throwaway git repositories; no network.

## License

MIT. Copyright 2026 Taktek and Nizar Mahmoud.
