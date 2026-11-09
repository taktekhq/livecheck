package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	maxBody    = 5 << 20 // bytes read from a response
	snippetLen = 200     // bytes of a response shown in a failure
)

var userAgent = "livecheck/" + version + " (+https://github.com/taktekhq/livecheck)"

// Outcome of one check.
const (
	Pass = "pass"
	Fail = "fail"
	Warn = "warn" // failed, but a branch it depends on moved less than grace ago: may still be deploying
)

// Result is what one check produced.
type Result struct {
	Name       string `json:"name"`
	URL        string `json:"url"`
	Result     string `json:"result"`
	HTTPStatus int    `json:"http_status,omitempty"`
	MS         int64  `json:"ms"`
	Burst      int    `json:"burst,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// Runner runs plans. Now and Client are swappable for tests.
type Runner struct {
	Client *http.Client
	Now    func() time.Time
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Run runs one plan: Burst requests at once, every one must pass.
func (r *Runner) Run(ctx context.Context, p Plan) Result {
	res := Result{Name: p.Name, URL: p.RawURL}
	if p.Burst > 1 {
		res.Burst = p.Burst
	}
	type one struct {
		status int
		ms     int64
		reason string
	}
	out := make([]one, p.Burst)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range out {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			out[i].status, out[i].ms, out[i].reason = r.request(ctx, p)
		}(i)
	}
	close(start)
	wg.Wait()

	failed, first := 0, -1
	for i, o := range out {
		if o.ms > res.MS {
			res.MS = o.ms
		}
		if o.reason != "" {
			failed++
			if first < 0 {
				first = i
			}
		}
	}
	if failed == 0 {
		res.Result = Pass
		res.HTTPStatus = out[0].status
		return res
	}
	res.Result = Fail
	res.HTTPStatus = out[first].status
	res.Reason = out[first].reason
	if p.Burst > 1 {
		res.Reason = fmt.Sprintf("%d of %d failed, first: %s", failed, p.Burst, res.Reason)
	}
	if p.Grace > 0 {
		if ref, age, ok := youngest(p.Refs, r.now()); ok && age < p.Grace {
			res.Result = Warn
			res.Reason += fmt.Sprintf(" (%s moved %s ago, within grace %s: may still be deploying)", ref, roundAge(age), shortDur(p.Grace))
		}
	}
	return res
}

func youngest(refs map[string]time.Time, now time.Time) (string, time.Duration, bool) {
	best, bestAge, ok := "", time.Duration(math.MaxInt64), false
	for _, name := range sortedKeys(refs) {
		age := now.Sub(refs[name])
		if age < 0 {
			age = 0
		}
		if age < bestAge {
			best, bestAge, ok = name, age, true
		}
	}
	return best, bestAge, ok
}

// request sends one request and returns the HTTP status, the time taken and why it failed ("" if it passed).
func (r *Runner) request(ctx context.Context, p Plan) (int, int64, string) {
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	var body io.Reader
	if p.Body != "" {
		body = strings.NewReader(p.Body)
	}
	req, err := http.NewRequestWithContext(ctx, p.Method, p.URL, body)
	if err != nil {
		return 0, 0, "bad request: " + err.Error()
	}
	req.Header.Set("User-Agent", userAgent)
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
	client := r.Client
	if client == nil {
		client = http.DefaultClient
	}
	if !p.Follow {
		c := *client
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		client = &c
	}

	t0 := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, ms(time.Since(t0)), netReason(err, p.Timeout)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	resp.Body.Close()
	took := ms(time.Since(t0))
	if err != nil {
		return resp.StatusCode, took, netReason(err, p.Timeout)
	}
	return resp.StatusCode, took, match(p, resp, data, took)
}

// match applies the expectations in order and returns the first failure.
func match(p Plan, resp *http.Response, data []byte, took int64) string {
	if resp.StatusCode != p.Status {
		return fmt.Sprintf("status %d, want %d%s", resp.StatusCode, p.Status, bodyTail(data, 0))
	}
	for _, x := range p.Contains {
		if !bytes.Contains(data, []byte(x.Value)) {
			return fmt.Sprintf("body lacks %q%s", x.Show, bodyTail(data, 0))
		}
	}
	for _, x := range p.NotContains {
		if i := bytes.Index(data, []byte(x.Value)); i >= 0 {
			return fmt.Sprintf("body has %q at byte %d%s", x.Show, i, bodyTail(data, i))
		}
	}
	for _, k := range sortedKeys(p.ResponseHeaders) {
		x := p.ResponseHeaders[k]
		got := resp.Header.Get(k)
		if !sameString(x.Value, got) {
			return fmt.Sprintf("header %s is %q, want %q", k, clip(got), x.Show)
		}
	}
	if p.JSON != nil {
		doc, err := decodeJSON(resp.Header.Get("Content-Type"), data)
		if err != nil {
			return fmt.Sprintf("body is not JSON (%v)%s", err, bodyTail(data, 0))
		}
		if msg := subset(p.JSON, p.JSONRaw, doc, "$"); msg != "" {
			return msg
		}
	}
	if p.MaxMS > 0 && took > int64(p.MaxMS) {
		return fmt.Sprintf("took %dms, max %dms", took, p.MaxMS)
	}
	return ""
}

// decodeJSON reads a JSON body, or the last data: event of a text/event-stream body (MCP servers
// may answer either way).
func decodeJSON(contentType string, data []byte) (any, error) {
	if strings.HasPrefix(strings.ToLower(contentType), "text/event-stream") {
		var last []byte
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 64<<10), maxBody)
		for sc.Scan() {
			if rest, ok := bytes.CutPrefix(sc.Bytes(), []byte("data:")); ok {
				last = append(last[:0], bytes.TrimSpace(rest)...)
			}
		}
		if last == nil {
			return nil, errors.New("event stream without a data: line")
		}
		data = last
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// subset reports where got differs from want. Objects match when every key in want matches (extra
// keys in got are fine); arrays match element by element and must have the same length; strings
// match exactly, or by prefix when both are commit ids (so a short id matches a full one).
func subset(want, raw, got any, path string) string {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return fmt.Sprintf("json %s is %s, want an object", path, describe(got))
		}
		rawMap, _ := raw.(map[string]any)
		for _, k := range sortedKeys(w) {
			gv, ok := g[k]
			if !ok {
				return fmt.Sprintf("json %s has no key %q", path, k)
			}
			if msg := subset(w[k], rawMap[k], gv, path+"."+k); msg != "" {
				return msg
			}
		}
		return ""
	case []any:
		g, ok := got.([]any)
		if !ok {
			return fmt.Sprintf("json %s is %s, want an array", path, describe(got))
		}
		if len(g) != len(w) {
			return fmt.Sprintf("json %s has %d items, want %d", path, len(g), len(w))
		}
		rawList, _ := raw.([]any)
		for i := range w {
			var r any
			if i < len(rawList) {
				r = rawList[i]
			}
			if msg := subset(w[i], r, g[i], fmt.Sprintf("%s[%d]", path, i)); msg != "" {
				return msg
			}
		}
		return ""
	case string:
		g, ok := got.(string)
		if !ok || !sameString(w, g) {
			show := w
			if rs, ok := raw.(string); ok && strings.Contains(rs, "{{env.") {
				show = rs
			}
			return fmt.Sprintf("json %s is %s, want %q", path, describe(got), show)
		}
		return ""
	case nil:
		if got != nil {
			return fmt.Sprintf("json %s is %s, want null", path, describe(got))
		}
		return ""
	case bool:
		if g, ok := got.(bool); !ok || g != w {
			return fmt.Sprintf("json %s is %s, want %v", path, describe(got), w)
		}
		return ""
	}
	if wn, ok := number(want); ok {
		n, isNum := got.(json.Number)
		if isNum {
			if gf, err := n.Float64(); err == nil && gf == wn {
				return ""
			}
		}
		return fmt.Sprintf("json %s is %s, want %v", path, describe(got), want)
	}
	return fmt.Sprintf("json %s: cannot compare with %T", path, want)
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

func describe(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return fmt.Sprintf("%q", clip(t))
	case json.Number:
		return t.String()
	case bool:
		return fmt.Sprint(t)
	case map[string]any:
		return "an object"
	case []any:
		return fmt.Sprintf("an array of %d", len(t))
	}
	return fmt.Sprintf("%v", v)
}

// sameString is equality, except that two commit ids (hex, 7 or more characters) match when the
// shorter is a prefix of the longer.
func sameString(want, got string) bool {
	if want == got {
		return true
	}
	if isCommitID(want) && isCommitID(got) {
		a, b := strings.ToLower(want), strings.ToLower(got)
		if len(a) > len(b) {
			a, b = b, a
		}
		return strings.HasPrefix(b, a)
	}
	return false
}

func isCommitID(s string) bool {
	if len(s) < 7 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// bodyTail shows up to snippetLen bytes of the body starting at from, on one line.
func bodyTail(data []byte, from int) string {
	if len(data) == 0 {
		return ": empty body"
	}
	if from > len(data) {
		from = len(data)
	}
	return ": " + oneLine(data[from:])
}

func oneLine(b []byte) string {
	cut := len(b) > snippetLen
	if cut {
		b = b[:snippetLen]
		for len(b) > 0 && !utf8.Valid(b) {
			b = b[:len(b)-1]
		}
	}
	var sb strings.Builder
	space := false
	for _, r := range string(b) {
		if r < 0x20 || r == 0x7f || r == utf8.RuneError {
			r = ' '
		}
		if r == ' ' {
			if space {
				continue
			}
			space = true
		} else {
			space = false
		}
		sb.WriteRune(r)
	}
	s := strings.TrimSpace(sb.String())
	if cut {
		s += "..."
	}
	return s
}

func clip(s string) string {
	if len(s) <= snippetLen {
		return s
	}
	return oneLine([]byte(s))
}

// netReason turns a transport error into a short reason without the URL (which may hold a token).
func netReason(err error, timeout time.Duration) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("timeout after %s", shortDur(timeout))
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return fmt.Sprintf("timeout after %s", shortDur(timeout))
		}
		err = ue.Err
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return fmt.Sprintf("timeout after %s", shortDur(timeout))
	}
	return err.Error()
}

func ms(d time.Duration) int64 { return d.Milliseconds() }

// shortDur prints 2h instead of 2h0m0s and 15m instead of 15m0s.
func shortDur(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return s
}

func roundAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return d.Round(time.Second).String()
	case d < time.Hour:
		return shortDur(d.Round(time.Minute))
	}
	return shortDur(d.Round(10 * time.Minute))
}

// Summary counts results.
type Summary struct {
	Passed int `json:"passed"`
	Failed int `json:"failed"`
	Warned int `json:"warned"`
}

func summarize(rs []Result) Summary {
	var s Summary
	for _, r := range rs {
		switch r.Result {
		case Pass:
			s.Passed++
		case Warn:
			s.Warned++
		default:
			s.Failed++
		}
	}
	return s
}

// worst is the outcome of a set: any fail, else any warn, else pass.
func (s Summary) worst() string {
	switch {
	case s.Failed > 0:
		return Fail
	case s.Warned > 0:
		return Warn
	}
	return Pass
}
