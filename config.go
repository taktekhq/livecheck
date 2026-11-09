package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultTimeout = 10 * time.Second
	maxTimeout     = 2 * time.Minute
	maxBurst       = 20
)

// Config is one livecheck.yaml.
type Config struct {
	Name   string  `yaml:"name"`
	Checks []Check `yaml:"checks"`
}

// Check is one entry under checks:, as written in the file.
type Check struct {
	Name            string            `yaml:"name"`
	URL             string            `yaml:"url"`
	Method          string            `yaml:"method"`
	Headers         map[string]string `yaml:"headers"`
	Body            string            `yaml:"body"`
	Status          int               `yaml:"status"`
	Contains        Strings           `yaml:"contains"`
	NotContains     Strings           `yaml:"not_contains"`
	JSON            any               `yaml:"json"`
	ResponseHeaders map[string]string `yaml:"response_headers"`
	MaxMS           int               `yaml:"max_ms"`
	Timeout         Duration          `yaml:"timeout"`
	Burst           int               `yaml:"burst"`
	Grace           Duration          `yaml:"grace"`
	FollowRedirects *bool             `yaml:"follow_redirects"`
}

// Strings accepts either one string or a list of strings.
type Strings []string

func (s *Strings) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		*s = Strings{n.Value}
		return nil
	case yaml.SequenceNode:
		var list []string
		if err := n.Decode(&list); err != nil {
			return err
		}
		*s = list
		return nil
	}
	return fmt.Errorf("line %d: want a string or a list of strings", n.Line)
}

// Duration accepts Go durations such as 90s, 15m or 2h.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %q is not a duration (use 30s, 15m, 2h)", n.Line, n.Value)
	}
	if v < 0 {
		return fmt.Errorf("line %d: duration %q is negative", n.Line, n.Value)
	}
	*d = Duration(v)
	return nil
}

// ParseConfig reads YAML. Unknown fields are errors, so a typo cannot silently skip a check.
func ParseConfig(data []byte) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("the file is empty")
		}
		return nil, err
	}
	if len(c.Checks) == 0 {
		return nil, errors.New("no checks: add a checks: list")
	}
	return &c, nil
}

// Resolver answers the {{git.*}} and {{env.*}} templates.
type Resolver interface {
	// Ref returns the full commit id of the branch (origin/<name> when there is a remote) and its
	// commit time.
	Ref(name string) (sha string, at time.Time, err error)
	Env(name string) (string, bool)
}

// Plan is a check ready to run: defaults filled in and templates expanded.
type Plan struct {
	Name            string
	RawURL          string // as written, for output: an {{env.X}} in it is never printed expanded
	URL             string
	Method          string
	Headers         map[string]string
	Body            string
	Status          int
	Contains        []Expect
	NotContains     []Expect
	JSON            any // expected value with templates expanded
	JSONRaw         any // expected value as written
	ResponseHeaders map[string]Expect
	MaxMS           int
	Timeout         time.Duration
	Burst           int
	Grace           time.Duration
	Follow          bool
	Refs            map[string]time.Time // git refs the check depends on, with their commit times
}

// Expect is an expected string, expanded, plus what to show in a failure message.
type Expect struct {
	Value string
	Show  string
}

var tmplRE = regexp.MustCompile(`\{\{\s*([^{}]*?)\s*\}\}`)
var refRE = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

type expander struct {
	r      Resolver
	refs   map[string]time.Time
	secret bool // the last expansion used {{env.X}}
}

func (e *expander) expand(s string) (string, error) {
	e.secret = false
	var firstErr error
	out := tmplRE.ReplaceAllStringFunc(s, func(m string) string {
		key := tmplRE.FindStringSubmatch(m)[1]
		v, err := e.lookup(key)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		return v
	})
	return out, firstErr
}

func (e *expander) lookup(key string) (string, error) {
	switch {
	case strings.HasPrefix(key, "git."):
		ref := strings.TrimPrefix(key, "git.")
		short := false
		if strings.HasSuffix(ref, ".short") {
			ref, short = strings.TrimSuffix(ref, ".short"), true
		}
		if ref == "" || !refRE.MatchString(ref) || strings.Contains(ref, "..") || strings.HasPrefix(ref, "-") {
			return "", fmt.Errorf("{{%s}}: %q is not a branch name", key, ref)
		}
		if e.r == nil {
			return "", fmt.Errorf("{{%s}}: no git repository to read it from", key)
		}
		sha, at, err := e.r.Ref(ref)
		if err != nil {
			return "", fmt.Errorf("{{%s}}: %v", key, err)
		}
		e.refs[ref] = at
		if short && len(sha) > 7 {
			sha = sha[:7]
		}
		return sha, nil
	case strings.HasPrefix(key, "env."):
		name := strings.TrimPrefix(key, "env.")
		e.secret = true
		if e.r == nil {
			return "", fmt.Errorf("{{%s}}: no environment", key)
		}
		v, ok := e.r.Env(name)
		if !ok {
			return "", fmt.Errorf("{{%s}}: environment variable %s is not set", key, name)
		}
		return v, nil
	}
	return "", fmt.Errorf("{{%s}}: unknown template (use {{git.main}}, {{git.main.short}} or {{env.NAME}})", key)
}

func (e *expander) expect(s string) (Expect, error) {
	v, err := e.expand(s)
	if err != nil {
		return Expect{}, err
	}
	show := v
	if e.secret {
		show = s
	}
	return Expect{Value: v, Show: show}, nil
}

func (e *expander) expandAny(v any) (any, error) {
	switch t := v.(type) {
	case string:
		return e.expand(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			y, err := e.expandAny(x)
			if err != nil {
				return nil, err
			}
			out[k] = y
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			y, err := e.expandAny(x)
			if err != nil {
				return nil, err
			}
			out[i] = y
		}
		return out, nil
	}
	return v, nil
}

// Plans validates the config and expands every template. All errors are config errors.
func (c *Config) Plans(r Resolver) ([]Plan, error) {
	seen := map[string]bool{}
	plans := make([]Plan, 0, len(c.Checks))
	for i, ch := range c.Checks {
		p, err := ch.plan(r)
		if err != nil {
			label := ch.Name
			if label == "" {
				label = fmt.Sprintf("#%d", i+1)
			}
			return nil, fmt.Errorf("check %s: %v", label, err)
		}
		if seen[p.Name] {
			return nil, fmt.Errorf("check %s: the name is used twice", p.Name)
		}
		seen[p.Name] = true
		plans = append(plans, p)
	}
	return plans, nil
}

func (ch Check) plan(r Resolver) (Plan, error) {
	e := &expander{r: r, refs: map[string]time.Time{}}
	p := Plan{
		Name:    strings.TrimSpace(ch.Name),
		RawURL:  ch.URL,
		Method:  strings.ToUpper(strings.TrimSpace(ch.Method)),
		Status:  ch.Status,
		MaxMS:   ch.MaxMS,
		Timeout: time.Duration(ch.Timeout),
		Burst:   ch.Burst,
		Grace:   time.Duration(ch.Grace),
		Follow:  ch.FollowRedirects == nil || *ch.FollowRedirects,
	}
	if p.Name == "" {
		return p, errors.New("name is required")
	}
	if ch.URL == "" {
		return p, errors.New("url is required")
	}
	var err error
	if p.URL, err = e.expand(ch.URL); err != nil {
		return p, err
	}
	u, err := url.Parse(p.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return p, fmt.Errorf("url %q must be an http or https address", ch.URL)
	}
	if p.Method == "" {
		p.Method = "GET"
		if ch.Body != "" {
			p.Method = "POST"
		}
	}
	if p.Status == 0 {
		p.Status = 200
	}
	if p.Status < 100 || p.Status > 599 {
		return p, fmt.Errorf("status %d is not an HTTP status", p.Status)
	}
	if p.MaxMS < 0 {
		return p, errors.New("max_ms is negative")
	}
	if p.Timeout == 0 {
		p.Timeout = defaultTimeout
	}
	if p.Timeout > maxTimeout {
		return p, fmt.Errorf("timeout %s is over the %s ceiling", p.Timeout, maxTimeout)
	}
	if p.Burst == 0 {
		p.Burst = 1
	}
	if p.Burst < 1 || p.Burst > maxBurst {
		return p, fmt.Errorf("burst %d must be between 1 and %d (more is a load test, not a check)", p.Burst, maxBurst)
	}
	if p.Body, err = e.expand(ch.Body); err != nil {
		return p, err
	}
	if len(ch.Headers) > 0 {
		p.Headers = map[string]string{}
		for _, k := range sortedKeys(ch.Headers) {
			if p.Headers[k], err = e.expand(ch.Headers[k]); err != nil {
				return p, err
			}
		}
	}
	for _, s := range ch.Contains {
		x, err := e.expect(s)
		if err != nil {
			return p, err
		}
		if x.Value == "" {
			return p, errors.New("contains has an empty string")
		}
		p.Contains = append(p.Contains, x)
	}
	for _, s := range ch.NotContains {
		x, err := e.expect(s)
		if err != nil {
			return p, err
		}
		if x.Value == "" {
			return p, errors.New("not_contains has an empty string")
		}
		p.NotContains = append(p.NotContains, x)
	}
	if len(ch.ResponseHeaders) > 0 {
		p.ResponseHeaders = map[string]Expect{}
		for _, k := range sortedKeys(ch.ResponseHeaders) {
			if p.ResponseHeaders[k], err = e.expect(ch.ResponseHeaders[k]); err != nil {
				return p, err
			}
		}
	}
	if ch.JSON != nil {
		p.JSONRaw = ch.JSON
		if p.JSON, err = e.expandAny(ch.JSON); err != nil {
			return p, err
		}
	}
	p.Refs = e.refs
	if p.Grace > 0 && len(p.Refs) == 0 {
		return p, errors.New("grace needs a {{git.<branch>}} template in the check: it is measured from that branch's last commit")
	}
	return p, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// osEnv is the default environment lookup.
func osEnv(name string) (string, bool) { return os.LookupEnv(name) }
