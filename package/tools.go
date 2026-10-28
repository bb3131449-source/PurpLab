package main

// purp-tools-v2
//
// PurpNet tool standard v2
//
// Layout   ~/PurpNet/package/tools/<id>.go. One file = one tool, package main, std-lib only.
//          Files starting with "_" or "." and *_test.go are ignored. <id> = file name without ".go".
// Manifest leading "// purp:key = value" comment lines, before "package". Keys:
//            name     menu label (default: <id>)
//            version  free text, e.g. "1.0.0 Release"
//            author   free text
//            desc     one-line description
//            pause    "key" (default): wait for a key after the tool; "none": return to the menu at once
//          Unknown keys are ignored.
// Build    host runs "go build" and caches the binary in <user-cache>/purpnet/tools/<id>-<sha256[:6]>.
//          The binary is exec'd directly (no "go run"): exit codes and signals are not rewritten.
// Runtime  stdin/stdout/stderr = the real tty, cooked mode (ECHO, ICANON, ISIG on).
//          The host stays in the alt screen: frame on rows 1..N, tool output below it.
//          The tool must not switch screens or clear the screen.
//          Env: PURP=1, PURP_API=2, PURP_TOOL=<id>, PURP_TOOLS_DIR=<dir>.
// Exit     0 = ok. 130 or death by SIGINT = interrupted (not an error). Anything else = failure.
// Signals  Ctrl+C reaches the host and the tool, the host stays alive. If the tool is still running
//          after 3s the host sends SIGTERM, after 2s more SIGKILL. A second Ctrl+C = SIGKILL at once.
//          SIGTERM/SIGHUP/SIGQUIT sent to the host are forwarded to the tool, then the host exits.

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	pauseKey  = "key"
	pauseNone = "none"

	termGrace   = 3 * time.Second
	killGrace   = 2 * time.Second
	settleDelay = 40 * time.Millisecond
)

type Tool struct {
	ID      string
	Path    string
	Name    string
	Version string
	Author  string
	Desc    string
	Pause   string
}

type Result struct {
	Code        int
	Dur         time.Duration
	Err         error
	Interrupted bool
	BuildFailed bool
	Sig         os.Signal // non-SIGINT signal received by the host: the host must exit
}

func toolsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "PurpNet", "package", "tools")
}

func cacheDir() string {
	d, err := os.UserCacheDir()
	if err != nil {
		d = os.TempDir()
	}
	return filepath.Join(d, "purpnet", "tools")
}

func LoadTools() []Tool {
	dir := toolsDir()
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var tools []Tool
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") ||
			strings.HasPrefix(n, "_") || strings.HasPrefix(n, ".") {
			continue
		}
		t, err := parseToolMeta(filepath.Join(dir, n))
		if err != nil {
			continue
		}
		t.ID = strings.TrimSuffix(n, ".go")
		if t.Name == "" {
			t.Name = t.ID
		}
		tools = append(tools, t)
	}
	sort.Slice(tools, func(i, j int) bool {
		return strings.ToLower(tools[i].Name) < strings.ToLower(tools[j].Name)
	})
	return tools
}

func parseToolMeta(path string) (Tool, error) {
	f, err := os.Open(path)
	if err != nil {
		return Tool{}, err
	}
	defer f.Close()
	t := Tool{Path: path, Pause: pauseKey}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "//") {
			break
		}
		rest, ok := strings.CutPrefix(strings.TrimSpace(strings.TrimPrefix(line, "//")), "purp:")
		if !ok {
			continue
		}
		k, v, ok := strings.Cut(rest, "=")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		switch strings.TrimSpace(k) {
		case "name":
			t.Name = v
		case "version":
			t.Version = v
		case "author":
			t.Author = v
		case "desc":
			t.Desc = v
		case "pause":
			if v == pauseKey || v == pauseNone {
				t.Pause = v
			}
		}
	}
	return t, sc.Err()
}

func (t Tool) binPath() (string, error) {
	src, err := os.ReadFile(t.Path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(src)
	return filepath.Join(cacheDir(), t.ID+"-"+hex.EncodeToString(sum[:6])), nil
}

// Build returns the path of an up-to-date binary, compiling only if the source hash changed.
func (t Tool) Build() (string, error) {
	bin, err := t.binPath()
	if err != nil {
		return "", err
	}
	if st, err := os.Stat(bin); err == nil && st.Mode().IsRegular() {
		return bin, nil
	}
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		return "", err
	}
	fmt.Printf("\x1b[2m· building %s\x1b[m\n", t.Name)
	tmp := bin + ".tmp"
	cmd := exec.Command("go", "build", "-o", tmp, t.Path)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("build failed: %w", err)
	}
	if err := os.Rename(tmp, bin); err != nil {
		return "", err
	}
	t.prune(bin)
	return bin, nil
}

// prune removes stale cached binaries of this tool only: "<id>-" + exactly 12 hex chars.
func (t Tool) prune(keep string) {
	ents, _ := os.ReadDir(cacheDir())
	for _, e := range ents {
		if rest, ok := strings.CutPrefix(e.Name(), t.ID+"-"); ok && len(rest) == 12 {
			if p := filepath.Join(cacheDir(), e.Name()); p != keep {
				os.Remove(p)
			}
		}
	}
}

// settle collects signals that reach the host a moment after the child died (tty delivers
// SIGINT to both at once). SIGINT never overrides a stronger signal.
func settle(sig <-chan os.Signal, got os.Signal) os.Signal {
	tm := time.NewTimer(settleDelay)
	defer tm.Stop()
	for {
		select {
		case s := <-sig:
			if got == nil || got == syscall.SIGINT {
				got = s
			}
		case <-tm.C:
			return got
		}
	}
}

func finish(res Result, got os.Signal) Result {
	switch {
	case got == nil:
	case got == syscall.SIGINT:
		if res.Err != nil {
			res.Interrupted, res.BuildFailed = true, false
		}
	default:
		res.Sig = got
	}
	return res
}

func (t Tool) Run(sig <-chan os.Signal) Result {
	bin, err := t.Build()
	if err != nil {
		return finish(Result{Code: 1, Err: err, BuildFailed: true}, settle(sig, nil))
	}
	cmd := exec.Command(bin)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), "PURP=1", "PURP_API=2", "PURP_TOOL="+t.ID, "PURP_TOOLS_DIR="+toolsDir())
	t0 := time.Now()
	if err := cmd.Start(); err != nil {
		return finish(Result{Code: 126, Err: err, BuildFailed: true}, settle(sig, nil))
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var (
		got          os.Signal
		werr         error
		termT, killT <-chan time.Time
	)
loop:
	for {
		select {
		case werr = <-done:
			break loop
		case s := <-sig:
			switch {
			case got == nil && s == syscall.SIGINT:
				got, termT = s, time.After(termGrace)
			case got == nil:
				got, termT = s, time.After(termGrace)
				cmd.Process.Signal(s)
			default:
				cmd.Process.Kill()
			}
		case <-termT:
			cmd.Process.Signal(syscall.SIGTERM)
			termT, killT = nil, time.After(killGrace)
		case <-killT:
			cmd.Process.Kill()
			killT = nil
		}
	}

	res := Result{Dur: time.Since(t0)}
	if werr != nil {
		res.Err, res.Code = werr, 1
		var ee *exec.ExitError
		if errors.As(werr, &ee) {
			res.Code = ee.ExitCode()
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				res.Code = 128 + int(ws.Signal())
			}
		}
	}
	if res.Code == 130 {
		res.Interrupted = true
	}
	return finish(res, settle(sig, got))
}

func (r Result) Status() string {
	d := r.Dur.Round(100 * time.Millisecond)
	switch {
	case r.Interrupted:
		return fmt.Sprintf("\x1b[33m■ Interrupted\x1b[m · %s", d)
	case r.BuildFailed:
		return fmt.Sprintf("\x1b[31m✗ %v\x1b[m", r.Err)
	case r.Err != nil:
		return fmt.Sprintf("\x1b[31m✗ Exit %d\x1b[m · %s", r.Code, d)
	default:
		return fmt.Sprintf("\x1b[32m✓ Done\x1b[m · %s", d)
	}
}

func (t Tool) Summary() string {
	var parts []string
	if t.Version != "" {
		parts = append(parts, t.Version)
	}
	if t.Author != "" {
		parts = append(parts, "by "+t.Author)
	}
	return strings.Join(parts, " • ")
}

// MakeSample creates hello.go when the tools dir has no tools.
func MakeSample() {
	dir := toolsDir()
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".go") {
			return
		}
	}
	body := `// purp:name = "hello"
// purp:version = "0.0.1 Alpha"
// purp:author = "purpnet"
// purp:desc = "sample tool"

package main

import (
	"fmt"
	"time"
)

func main() {
	fmt.Println("Hello from PurpNet tool!")
	fmt.Println("Press Ctrl+C to interrupt.")
	for i := 3; i > 0; i-- {
		fmt.Printf("%d...\n", i)
		time.Sleep(time.Second)
	}
	fmt.Println("Done.")
}
`
	_ = os.WriteFile(filepath.Join(dir, "hello.go"), []byte(body), 0o644)
}
