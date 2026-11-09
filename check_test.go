package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func runOnePlan(t *testing.T, yml string, r Resolver, now func() time.Time) Result {
	t.Helper()
	ps := plans(t, yml, r)
	return (&Runner{Client: &http.Client{}, Now: now}).Run(context.Background(), ps[0])
}

func server(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s.URL
}

func TestStatusAndBodySnippet(t *testing.T) {
	long := strings.Repeat("x", 500)
	u := server(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		fmt.Fprint(w, "line one\n\tline two "+long)
	})
	res := runOnePlan(t, "checks:\n  - name: a\n    url: "+u+"\n", nil, nil)
	if res.Result != Fail || res.HTTPStatus != 503 {
		t.Fatalf("%+v", res)
	}
	if !strings.HasPrefix(res.Reason, "status 503, want 200: line one line two xxx") {
		t.Errorf("reason: %q", res.Reason)
	}
	snippet := strings.TrimPrefix(res.Reason, "status 503, want 200: ")
	if len(snippet) > snippetLen+3 || !strings.HasSuffix(snippet, "...") {
		t.Errorf("snippet must be at most %d bytes and marked as cut: %d %q", snippetLen, len(snippet), snippet)
	}
}

func TestContainsAndNotContains(t *testing.T) {
	u := server(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"result":{"content":[{"text":"{\"text_ar\":\"x\"}"}],"isError":false}}`)
	})
	pass := runOnePlan(t, "checks:\n  - name: a\n    url: "+u+"\n    contains: [text_ar, isError]\n    not_contains: '\"isError\":true'\n", nil, nil)
	if pass.Result != Pass {
		t.Errorf("want pass: %+v", pass)
	}
	miss := runOnePlan(t, "checks:\n  - name: a\n    url: "+u+"\n    contains: '\"text_ar\"'\n", nil, nil)
	if miss.Result != Fail || !strings.HasPrefix(miss.Reason, `body lacks "\"text_ar\""`) {
		t.Errorf("want a clear miss: %+v", miss)
	}
	found := runOnePlan(t, "checks:\n  - name: a\n    url: "+u+"\n    not_contains: '\"isError\":false'\n", nil, nil)
	if found.Result != Fail || !strings.Contains(found.Reason, `body has "\"isError\":false" at byte 54: "isError":false}}`) {
		t.Errorf("want the failing part shown: %+v", found)
	}
}

func TestJSONSubset(t *testing.T) {
	body := `{"status":"ok","build":"` + sha1 + `","n":3,"f":1.5,"ok":true,"none":null,"list":[1,"a"],"obj":{"x":"y","z":1}}`
	u := server(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	})
	cases := []struct{ json, want string }{
		{`{status: ok}`, ""},
		{`{build: "12c2e85"}`, ""}, // a short commit id matches the full one
		{`{build: "` + sha1 + `"}`, ""},
		{`{n: 3, f: 1.5, ok: true, none: null, list: [1, a], obj: {x: y}}`, ""},
		{`{status: okay}`, `json $.status is "ok", want "okay"`},
		{`{status: o}`, `json $.status is "ok", want "o"`}, // plain strings never prefix-match
		{`{build: "12c2e8"}`, `want "12c2e8"`},             // 6 characters is not a commit id
		{`{build: "deadbeef"}`, `json $.build is "` + sha1 + `", want "deadbeef"`},
		{`{missing: 1}`, `json $ has no key "missing"`},
		{`{n: 4}`, `json $.n is 3, want 4`},
		{`{ok: false}`, `json $.ok is true, want false`},
		{`{none: 1}`, `json $.none is null, want 1`},
		{`{list: [1]}`, `json $.list has 2 items, want 1`},
		{`{list: [1, b]}`, `json $.list[1] is "a", want "b"`},
		{`{obj: {z: 2}}`, `json $.obj.z is 1, want 2`},
		{`{status: {a: 1}}`, `json $.status is "ok", want an object`},
	}
	for _, c := range cases {
		res := runOnePlan(t, "checks:\n  - name: a\n    url: "+u+"\n    json: "+c.json+"\n", nil, nil)
		if c.want == "" && res.Result != Pass {
			t.Errorf("%s: want pass, got %+v", c.json, res)
		}
		if c.want != "" && (res.Result != Fail || !strings.Contains(res.Reason, c.want)) {
			t.Errorf("%s: reason %q, want %q", c.json, res.Reason, c.want)
		}
	}
}

func TestJSONNotJSON(t *testing.T) {
	u := server(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "<html>") })
	res := runOnePlan(t, "checks:\n  - name: a\n    url: "+u+"\n    json: {a: 1}\n", nil, nil)
	if res.Result != Fail || !strings.HasPrefix(res.Reason, "body is not JSON") || !strings.HasSuffix(res.Reason, ": <html>") {
		t.Errorf("%+v", res)
	}
}

func TestEventStream(t *testing.T) {
	u := server(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message\ndata: {\"id\":0}\n\nevent: message\ndata: {\"jsonrpc\":\"2.0\",\"result\":{\"isError\":false}}\n\n")
	})
	res := runOnePlan(t, "checks:\n  - name: a\n    url: "+u+"\n    method: POST\n    json: {result: {isError: false}}\n", nil, nil)
	if res.Result != Pass {
		t.Errorf("%+v", res)
	}
}

func TestResponseHeadersAndUserAgent(t *testing.T) {
	var ua, auth atomic.Value
	u := server(t, func(w http.ResponseWriter, r *http.Request) {
		ua.Store(r.UserAgent())
		auth.Store(r.Header.Get("Authorization"))
		w.Header().Set("X-Build", sha1)
	})
	r := fakeResolver{refs: map[string]string{"main": sha1}, env: map[string]string{"T": "tok"}}
	res := runOnePlan(t, "checks:\n  - name: a\n    url: "+u+"\n    headers: {Authorization: 'Bearer {{env.T}}'}\n    response_headers: {X-Build: '{{git.main.short}}'}\n", r, nil)
	if res.Result != Pass {
		t.Errorf("%+v", res)
	}
	if got := ua.Load().(string); !strings.HasPrefix(got, "livecheck/"+version) {
		t.Errorf("user agent %q", got)
	}
	if auth.Load().(string) != "Bearer tok" {
		t.Errorf("header not sent: %v", auth.Load())
	}
	bad := runOnePlan(t, "checks:\n  - name: a\n    url: "+u+"\n    response_headers: {X-Build: abc}\n", nil, nil)
	if bad.Result != Fail || !strings.Contains(bad.Reason, `header X-Build is "`+sha1+`", want "abc"`) {
		t.Errorf("%+v", bad)
	}
}

func TestMaxMSAndTimeout(t *testing.T) {
	u := server(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(300 * time.Millisecond):
		case <-r.Context().Done():
		}
		fmt.Fprint(w, "ok")
	})
	slow := runOnePlan(t, "checks:\n  - name: a\n    url: "+u+"\n    max_ms: 100\n", nil, nil)
	if slow.Result != Fail || !strings.HasPrefix(slow.Reason, "took ") || !strings.HasSuffix(slow.Reason, "max 100ms") {
		t.Errorf("max_ms: %+v", slow)
	}
	start := time.Now()
	to := runOnePlan(t, "checks:\n  - name: a\n    url: "+u+"\n    timeout: 50ms\n", nil, nil)
	if to.Result != Fail || to.Reason != "timeout after 50ms" {
		t.Errorf("timeout: %+v", to)
	}
	if time.Since(start) > 250*time.Millisecond {
		t.Errorf("the timeout did not cut the request: %s", time.Since(start))
	}
}

func TestBurst(t *testing.T) {
	var inFlight, peak, n int32
	var mu sync.Mutex
	u := server(t, func(w http.ResponseWriter, r *http.Request) {
		cur := atomic.AddInt32(&inFlight, 1)
		mu.Lock()
		if cur > peak {
			peak = cur
		}
		mu.Unlock()
		time.Sleep(100 * time.Millisecond)
		atomic.AddInt32(&inFlight, -1)
		if r.URL.Query().Get("limit") != "" && atomic.AddInt32(&n, 1)%3 == 0 {
			w.WriteHeader(429)
			fmt.Fprint(w, "slow down")
			return
		}
		fmt.Fprint(w, "ok")
	})
	ok := runOnePlan(t, "checks:\n  - name: a\n    url: "+u+"\n    burst: 6\n    contains: ok\n", nil, nil)
	if ok.Result != Pass || ok.Burst != 6 {
		t.Errorf("burst pass: %+v", ok)
	}
	if peak != 6 {
		t.Errorf("burst requests must run at once: peak %d in flight, want 6", peak)
	}
	bad := runOnePlan(t, "checks:\n  - name: a\n    url: "+u+"?limit=1\n    burst: 6\n", nil, nil)
	if bad.Result != Fail || bad.Reason != "2 of 6 failed, first: status 429, want 200: slow down" || bad.HTTPStatus != 429 {
		t.Errorf("burst fail: %+v", bad)
	}
}

func TestGrace(t *testing.T) {
	u := server(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"build":"0000000aaaa"}`) })
	merged := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	r := fakeResolver{refs: map[string]string{"main": sha1}, at: merged}
	yml := "checks:\n  - name: build\n    url: " + u + "\n    json: {build: '{{git.main}}'}\n    grace: 2h\n"

	young := runOnePlan(t, yml, r, func() time.Time { return merged.Add(14 * time.Minute) })
	if young.Result != Warn || !strings.HasSuffix(young.Reason, "(main moved 14m ago, within grace 2h: may still be deploying)") {
		t.Errorf("young merge: %+v", young)
	}
	old := runOnePlan(t, yml, r, func() time.Time { return merged.Add(3 * time.Hour) })
	if old.Result != Fail || strings.Contains(old.Reason, "grace") {
		t.Errorf("old merge: %+v", old)
	}
	if !strings.Contains(old.Reason, `json $.build is "0000000aaaa", want "`+sha1+`"`) {
		t.Errorf("reason: %q", old.Reason)
	}
	noGrace := runOnePlan(t, "checks:\n  - name: build\n    url: "+u+"\n    json: {build: '{{git.main}}'}\n", r,
		func() time.Time { return merged.Add(time.Minute) })
	if noGrace.Result != Fail {
		t.Errorf("without grace a fresh merge still fails: %+v", noGrace)
	}
}

func TestRedirects(t *testing.T) {
	u := server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			http.Redirect(w, r, "/new", http.StatusMovedPermanently)
			return
		}
		fmt.Fprint(w, "new page")
	})
	follow := runOnePlan(t, "checks:\n  - name: a\n    url: "+u+"/old\n    contains: new page\n", nil, nil)
	if follow.Result != Pass {
		t.Errorf("follow: %+v", follow)
	}
	stay := runOnePlan(t, "checks:\n  - name: a\n    url: "+u+"/old\n    status: 301\n    follow_redirects: false\n", nil, nil)
	if stay.Result != Pass {
		t.Errorf("no follow: %+v", stay)
	}
}

func TestConnectErrorHidesURL(t *testing.T) {
	s := httptest.NewServer(http.NotFoundHandler())
	addr := s.URL
	s.Close()
	r := fakeResolver{env: map[string]string{"K": "topsecret"}}
	res := runOnePlan(t, "checks:\n  - name: a\n    url: "+addr+"/?key={{env.K}}\n", r, nil)
	if res.Result != Fail || strings.Contains(res.Reason, "topsecret") || strings.Contains(res.Reason, "http://") {
		t.Errorf("reason must not carry the URL: %+v", res)
	}
	if res.URL != addr+"/?key={{env.K}}" {
		t.Errorf("the reported URL is the one as written: %s", res.URL)
	}
}

func TestSameString(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"ok", "ok", true},
		{"ok", "okay", false},
		{"12c2e85", sha1, true},
		{sha1, "12C2E85", true},
		{"12c2e8", sha1, false},
		{"12c2e86", sha1, false},
		{"release-1", "release-10", false},
	}
	for _, c := range cases {
		if got := sameString(c.a, c.b); got != c.want {
			t.Errorf("sameString(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

func TestShortDur(t *testing.T) {
	for d, want := range map[time.Duration]string{
		2 * time.Hour: "2h", 90 * time.Minute: "1h30m", 15 * time.Minute: "15m", 90 * time.Second: "1m30s",
		50 * time.Millisecond: "50ms", 10 * time.Second: "10s",
	} {
		if got := shortDur(d); got != want {
			t.Errorf("shortDur(%s) = %s, want %s", d, got, want)
		}
	}
}
