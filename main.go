// livecheck reads a livecheck.yaml and checks that production shows what it says: pages, APIs,
// MCP tools, and that the deployed build is the branch's latest commit.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const version = "0.1.0"

// ConfigName is the file livecheck looks for.
const ConfigName = "livecheck.yaml"

// Exit codes.
const (
	exitPass  = 0 // every check passed
	exitFail  = 1 // at least one check failed
	exitUsage = 2 // bad flags or a bad config file
	exitWarn  = 3 // nothing failed, but a check is within its grace (a deploy may still be running)
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "all":
			return runAll(args[1:], stdout, stderr)
		case "version", "-version", "--version":
			fmt.Fprintln(stdout, "livecheck", version)
			return exitPass
		case "help", "-h", "-help", "--help":
			fmt.Fprint(stdout, usageText)
			return exitPass
		}
	}
	return runOne(args, stdout, stderr)
}

const usageText = `livecheck checks that production shows what livecheck.yaml says.

Usage:
  livecheck [-f FILE] [-json] [-o FILE] [-no-fetch]
        run ./livecheck.yaml (or the nearest one up to the repository root)
  livecheck all [-json] [-o FILE] [-depth 2] [-ref BRANCH] [-no-fetch] [-v] [DIR]
        run the livecheck.yaml of every git repository under DIR, as committed on its main branch
  livecheck version

Exit status: 0 every check passed, 1 a check failed, 2 bad usage or config,
3 nothing failed but a check is within its grace (a deploy may still be running).
`

func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usageText) }
	return fs
}

func runOne(args []string, stdout, stderr io.Writer) int {
	fs := newFlags("livecheck", stderr)
	file := fs.String("f", "", "config file (default: ./"+ConfigName+" or the nearest parent up to the repository root)")
	asJSON := fs.Bool("json", false, "print JSON")
	outFile := fs.String("o", "", "also write the report to this file")
	noFetch := fs.Bool("no-fetch", false, "do not git fetch before reading {{git.<branch>}}")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "livecheck: unexpected argument %q (did you mean livecheck all?)\n", fs.Arg(0))
		return exitUsage
	}
	path := *file
	if path == "" {
		p, err := findConfig(".")
		if err != nil {
			fmt.Fprintln(stderr, "livecheck:", err)
			return exitUsage
		}
		path = p
	}
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(stderr, "livecheck:", err)
		return exitUsage
	}
	abs, _ := filepath.Abs(path)
	res := &GitResolver{Dir: filepath.Dir(abs), Fetch: !*noFetch}
	rep := checkFile(context.Background(), data, res)
	rep.File = displayPath(path)
	if rep.Error != "" {
		fmt.Fprintf(stderr, "livecheck: %s: %s\n", rep.File, rep.Error)
		return exitUsage
	}
	var buf bytes.Buffer
	if *asJSON {
		writeJSON(&buf, rep)
	} else {
		writeChecks(&buf, rep, "")
		fmt.Fprintf(&buf, "livecheck: %d passed, %d failed, %d warned\n", rep.Passed, rep.Failed, rep.Warned)
	}
	if !emit(stdout, stderr, &buf, *outFile) {
		return exitUsage
	}
	return exitCode(rep.Summary)
}

// FileReport is the result of one config file.
type FileReport struct {
	Repo   string `json:"repo,omitempty"`
	Path   string `json:"path,omitempty"`
	File   string `json:"file,omitempty"`
	Source string `json:"source,omitempty"`
	Result string `json:"result"`
	Error  string `json:"error,omitempty"`
	Summary
	Notes  []string `json:"notes,omitempty"`
	Checks []Result `json:"checks"`
}

// checkFile parses, plans and runs one config. A config problem ends up in Error.
func checkFile(ctx context.Context, data []byte, res *GitResolver) *FileReport {
	rep := &FileReport{Checks: []Result{}}
	cfg, err := ParseConfig(data)
	if err == nil {
		var plans []Plan
		plans, err = cfg.Plans(res)
		if err == nil {
			r := &Runner{Client: &http.Client{}}
			for _, p := range plans {
				rep.Checks = append(rep.Checks, r.Run(ctx, p))
			}
		}
	}
	rep.Notes = res.Notes
	if err != nil {
		rep.Error = err.Error()
		rep.Result = Fail
		return rep
	}
	rep.Summary = summarize(rep.Checks)
	rep.Result = rep.Summary.worst()
	return rep
}

func exitCode(s Summary) int {
	switch s.worst() {
	case Fail:
		return exitFail
	case Warn:
		return exitWarn
	}
	return exitPass
}

func tag(result string) string {
	switch result {
	case Pass:
		return "PASS"
	case Warn:
		return "WARN"
	case "skip":
		return "SKIP"
	}
	return "FAIL"
}

// writeChecks prints one line per check, then the notes.
func writeChecks(w io.Writer, rep *FileReport, indent string) {
	width := 0
	labels := make([]string, len(rep.Checks))
	for i, c := range rep.Checks {
		labels[i] = c.Name
		if c.Burst > 1 {
			labels[i] += fmt.Sprintf(" x%d", c.Burst)
		}
		if n := len([]rune(labels[i])); n > width {
			width = n
		}
	}
	if width > 40 {
		width = 40
	}
	for i, c := range rep.Checks {
		code := "---"
		if c.HTTPStatus > 0 {
			code = fmt.Sprint(c.HTTPStatus)
		}
		line := fmt.Sprintf("%s%s  %-*s  %s  %5dms", indent, tag(c.Result), width, labels[i], code, c.MS)
		if c.Reason != "" {
			line += "  " + c.Reason
		}
		fmt.Fprintln(w, line)
	}
	for _, n := range rep.Notes {
		fmt.Fprintf(w, "%snote: %s\n", indent, n)
	}
}

func writeJSON(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// emit prints the report and, with -o, writes it to a file (atomically, creating the folder).
func emit(stdout, stderr io.Writer, buf *bytes.Buffer, outFile string) bool {
	if _, err := stdout.Write(buf.Bytes()); err != nil {
		return false
	}
	if outFile == "" {
		return true
	}
	if err := writeFileAtomic(outFile, buf.Bytes()); err != nil {
		fmt.Fprintln(stderr, "livecheck:", err)
		return false
	}
	return true
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".livecheck-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func displayPath(p string) string {
	if wd, err := os.Getwd(); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			if rel, err := filepath.Rel(wd, abs); err == nil && !strings.HasPrefix(rel, "..") {
				return rel
			}
			return abs
		}
	}
	return p
}

// AllReport is the result of livecheck all.
type AllReport struct {
	Dir string    `json:"dir"`
	At  time.Time `json:"at"`
	Summary
	Skipped int           `json:"skipped"`
	Repos   []*FileReport `json:"repos"`
}

func runAll(args []string, stdout, stderr io.Writer) int {
	fs := newFlags("livecheck all", stderr)
	asJSON := fs.Bool("json", false, "print JSON")
	outFile := fs.String("o", "", "also write the report to this file")
	depth := fs.Int("depth", 2, "how many folder levels below DIR to look for repositories")
	ref := fs.String("ref", "", "branch whose committed livecheck.yaml is used (default: origin/HEAD, else main)")
	noFetch := fs.Bool("no-fetch", false, "do not git fetch; use the local origin/<branch>")
	verbose := fs.Bool("v", false, "print every check, not only the ones that did not pass")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(stderr, "livecheck all: give one folder")
		return exitUsage
	}
	dir := "."
	if fs.NArg() == 1 {
		dir = fs.Arg(0)
	}
	if *ref != "" && !refRE.MatchString(*ref) {
		fmt.Fprintf(stderr, "livecheck all: %q is not a branch name\n", *ref)
		return exitUsage
	}
	repos, err := discover(dir, *depth, *ref)
	if err != nil {
		fmt.Fprintln(stderr, "livecheck all:", err)
		return exitUsage
	}
	abs, _ := filepath.Abs(dir)
	all := &AllReport{Dir: abs, At: time.Now().UTC(), Repos: []*FileReport{}}
	for _, r := range repos {
		rep := r.check(context.Background(), !*noFetch)
		all.Repos = append(all.Repos, rep)
		switch rep.Result {
		case Pass:
			all.Passed++
		case Warn:
			all.Warned++
		case "skip":
			all.Skipped++
		default:
			all.Failed++
		}
	}

	var buf bytes.Buffer
	if *asJSON {
		writeJSON(&buf, all)
	} else {
		writeAll(&buf, all, *verbose)
	}
	if !emit(stdout, stderr, &buf, *outFile) {
		return exitUsage
	}
	return exitCode(all.Summary)
}

func writeAll(w io.Writer, all *AllReport, verbose bool) {
	fmt.Fprintf(w, "livecheck %s: %s, %s\n", version, all.Dir, all.At.Format("2006-01-02 15:04 MST"))
	if len(all.Repos) == 0 {
		fmt.Fprintf(w, "no repository under %s has a %s\n", all.Dir, ConfigName)
		return
	}
	width := 0
	for _, r := range all.Repos {
		if n := len([]rune(r.Repo)); n > width {
			width = n
		}
	}
	for _, r := range all.Repos {
		var what string
		switch {
		case r.Result == "skip":
			what = r.Error
		case r.Error != "":
			what = "config: " + r.Error
		default:
			what = fmt.Sprintf("%d/%d passed", r.Passed, len(r.Checks))
			if r.Warned > 0 {
				what += fmt.Sprintf(", %d within grace", r.Warned)
			}
		}
		fmt.Fprintf(w, "%s  %-*s  %s\n", tag(r.Result), width, r.Repo, what)
		if verbose || r.Result == Fail || r.Result == Warn {
			sub := &FileReport{Notes: r.Notes}
			for _, c := range r.Checks {
				if verbose || c.Result != Pass {
					sub.Checks = append(sub.Checks, c)
				}
			}
			writeChecks(w, sub, "      ")
		}
	}
	fmt.Fprintf(w, "livecheck all: %d repos, %d passed, %d failed, %d warned, %d skipped\n",
		len(all.Repos), all.Passed, all.Failed, all.Warned, all.Skipped)
}
