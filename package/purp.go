package main

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
	"unsafe"
)

type RGB [3]int

type Cell struct {
	ch           rune
	gray, target RGB
	start        float64
}

type Logo struct {
	rows [][]rune
	w, h int
}

const (
	fps         = 60
	appearDur   = 0.2
	appearLag   = 0.05
	colorDelay  = appearLag + appearDur
	colorDur    = 0.6
	sweepSpread = 0.65
	sweepJitter = 0.07
	glowAmount  = 0.5
	glowTint    = 0.3
	titleText   = "PurpNet!"
	titleDelay  = 0
	titleStep   = 0
	titleFade   = 0.35
	titleMid    = 0.5
	titleGap    = 2

	bgQueryTimeout = 180 * time.Millisecond

	tcsetsf = 0x5404
	tcflsh  = 0x540B
)

var (
	logo         = newLogo(" ▐▛██▜▌", "▝▜████▛▘", " ▐▌▐▌▐▌")
	titleRow     = 0
	versionRow   = 1
	white        = RGB{255, 255, 255}
	gray         = []RGB{{225, 225, 225}, {140, 140, 140}}
	color        = []RGB{{170, 170, 255}, {60, 30, 180}}
	glow         RGB
	titleDark    = RGB{14, 12, 20}
	titleRunes   = []rune(titleText)
	versionText  = loadVersion()
	versionRunes = []rune(versionText)
	baseEnd      = math.Max(
		colorDelay+sweepSpread+sweepJitter+colorDur,
		math.Max(
			titleDelay+float64(len(titleRunes)-1)*titleStep+titleFade,
			titleDelay+float64(max(len(versionRunes)-1, 0))*titleStep+titleFade,
		),
	)
	lin      [256]float64
	out      = os.Stdout
	exitKeys = map[byte]int{3: 130, 4: 0, 28: 131}
)

func init() {
	for i := range lin {
		lin[i] = math.Pow(float64(i)/255, 2.2)
	}
	glow = mix(white, color[0], glowTint)
}

func loadVersion() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, "PurpNet", "version.toml"))
	if err != nil {
		return ""
	}
	var v, st string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		k, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		switch k {
		case "version":
			v = val
		case "status":
			st = val
		}
	}
	if v == "" {
		return ""
	}
	if st == "" {
		return v
	}
	st = strings.ToUpper(st[:1]) + st[1:]
	return v + " • " + st
}

func newLogo(src ...string) Logo {
	l := Logo{h: len(src)}
	for _, s := range src {
		rs := []rune(s)
		l.rows = append(l.rows, rs)
		l.w = max(l.w, len(rs))
	}
	for i, rs := range l.rows {
		for len(rs) < l.w {
			rs = append(rs, ' ')
		}
		l.rows[i] = rs
	}
	return l
}

func clamp01(v float64) float64 { return max(0, min(1, v)) }

func smoothstep(x float64) float64 {
	x = clamp01(x)
	return x * x * (3 - 2*x)
}

func smootherstep(x float64) float64 {
	x = clamp01(x)
	return x * x * x * (x*(x*6-15) + 10)
}

func mix(a, b RGB, k float64) RGB {
	if k <= 0 {
		return a
	}
	if k >= 1 {
		return b
	}
	var o RGB
	for i := range o {
		la, lb := lin[a[i]], lin[b[i]]
		o[i] = int(math.Round(255 * math.Pow(clamp01(la+(lb-la)*k), 1/2.2)))
	}
	return o
}

func stopsAt(stops []RGB, t float64) RGB {
	t = clamp01(t) * float64(len(stops)-1)
	i := min(int(t), len(stops)-2)
	return mix(stops[i], stops[i+1], t-float64(i))
}

func diag(x, y int, wx float64) float64 {
	return clamp01(wx*float64(x)/float64(max(logo.w-1, 1)) + (1-wx)*float64(y)/float64(max(logo.h-1, 1)))
}

func jitter(x, y int) float64 {
	v := math.Sin(float64(x)*12.9898+float64(y)*78.233) * 43758.5453
	return v - math.Floor(v)
}

type Renderer struct {
	grid  [][]Cell
	buf   []byte
	bg    RGB
	plain bool
}

func newRenderer() *Renderer {
	r := &Renderer{bg: titleDark, grid: make([][]Cell, logo.h)}
	for y, row := range logo.rows {
		r.grid[y] = make([]Cell, logo.w)
		for x, ch := range row {
			c := Cell{ch: ch}
			if ch != ' ' {
				c.gray = stopsAt(gray, diag(x, y, 0.8))
				c.target = stopsAt(color, diag(x, y, 0.8))
				c.start = diag(x, y, 0.5)*sweepSpread + sweepJitter*jitter(x, y)
			}
			r.grid[y][x] = c
		}
	}
	return r
}

func (r Renderer) cell(c Cell, t float64) (RGB, bool) {
	if c.ch == ' ' {
		return RGB{}, false
	}
	// ВАЖНО: старый эффект — `c.start * appearLag`, а не `c.start + appearLag`.
	// Иначе клетки начинают перекрашиваться до того, как проявились серыми,
	// и эффект "сначала серый градиент, потом плавная перекраска" ломается.
	k := smoothstep((t - appearLag) / appearDur)
	if k <= 0 {
		return RGB{}, false
	}
	s := smootherstep((t - colorDelay - c.start) / colorDur)
	if s >= 1 {
		return c.target, true
	}
	o := mix(mix(r.bg, c.gray, k), c.target, s)
	if g := math.Sin(math.Pi * s); g > 0 {
		o = mix(o, glow, glowAmount*g)
	}
	return o, true
}

func (r Renderer) titleColor(i int, t float64) (RGB, bool) {
	u := (t - titleDelay - float64(i)*titleStep) / titleFade
	if u <= 0 {
		return RGB{}, false
	}
	k := smoothstep(u)
	pos := float64(i) / float64(max(len(titleRunes)-1, 1))
	end := stopsAt(color, pos)
	if k >= 1 {
		return end, true
	}
	return mix(r.bg, end, k), true
}

func (r Renderer) versionColor(i int, t float64) (RGB, bool) {
	u := (t - titleDelay - float64(i)*titleStep) / titleFade
	if u <= 0 {
		return RGB{}, false
	}
	k := smootherstep(u)
	if k >= 1 {
		return gray[0], true
	}
	if k < titleMid {
		return mix(r.bg, gray[1], k/titleMid), true
	}
	return mix(gray[1], gray[0], (k-titleMid)/(1-titleMid)), true
}

func appendFG(b []byte, c RGB) []byte {
	return fmt.Appendf(b, "\x1b[38;2;%d;%d;%dm", c[0], c[1], c[2])
}

func (r *Renderer) esc(b []byte, s string) []byte {
	if r.plain {
		return b
	}
	return append(b, s...)
}

func (r *Renderer) eol(b []byte) []byte { return append(r.esc(b, "\x1b[K"), '\n') }

func (r *Renderer) paint(b []byte, n int, at func(i int) (rune, RGB, bool)) []byte {
	var last RGB
	have := false
	for i := 0; i < n; i++ {
		ch, col, ok := at(i)
		if !ok {
			b = append(b, ' ')
			continue
		}
		if !r.plain && (!have || col != last) {
			b = appendFG(b, col)
			last, have = col, true
		}
		b = utf8.AppendRune(b, ch)
	}
	return r.esc(b, "\x1b[m")
}

func (r *Renderer) frame(t float64, up int) []byte {
	b := r.buf[:0]
	if r.plain {
		t = 1e9
	} else {
		b = append(b, "\x1b[?2026h"...)
		if up > 0 {
			b = fmt.Appendf(b, "\x1b[%dA\r", up)
		}
	}
	for y, row := range r.grid {
		b = r.paint(b, len(row), func(i int) (rune, RGB, bool) {
			col, ok := r.cell(row[i], t)
			return row[i].ch, col, ok
		})
		if y == titleRow {
			b = append(b, strings.Repeat(" ", titleGap)...)
			b = r.paint(b, len(titleRunes), func(i int) (rune, RGB, bool) {
				col, ok := r.titleColor(i, t)
				return titleRunes[i], col, ok
			})
		}
		if y == versionRow && len(versionRunes) > 0 {
			b = append(b, strings.Repeat(" ", titleGap)...)
			b = r.paint(b, len(versionRunes), func(i int) (rune, RGB, bool) {
				col, ok := r.versionColor(i, t)
				return versionRunes[i], col, ok
			})
		}
		b = r.eol(b)
	}
	b = r.esc(b, "\x1b[?2026l")
	r.buf = b
	return b
}

type Terminal struct {
	fd   int
	orig syscall.Termios
	raw  bool
	keys bool
}

func ioctl(fd int, req uintptr, t *syscall.Termios) bool {
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(unsafe.Pointer(t)))
	return e == 0
}

func isTTY(fd int) bool {
	var t syscall.Termios
	return ioctl(fd, syscall.TCGETS, &t)
}

func (t *Terminal) enterRaw() {
	if !ioctl(t.fd, syscall.TCGETS, &t.orig) {
		return
	}
	s := t.orig
	s.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	s.Iflag &^= syscall.IXON | syscall.ICRNL
	s.Cc[syscall.VMIN], s.Cc[syscall.VTIME] = 0, 0
	if t.raw = ioctl(t.fd, syscall.TCSETS, &s); t.raw {
		t.keys = true
		t.flush()
	}
}

func (t *Terminal) flush() {
	if t.raw {
		syscall.Syscall(syscall.SYS_IOCTL, uintptr(t.fd), tcflsh, 0)
	}
}

func (t *Terminal) read(p []byte) int {
	for {
		n, err := syscall.Read(t.fd, p)
		switch err {
		case nil:
			return n
		case syscall.EINTR:
		case syscall.EAGAIN:
			return 0
		default:
			return -1
		}
	}
}

func daDone(resp []byte) bool {
	i := bytes.LastIndex(resp, []byte("\x1b[?"))
	return i >= 0 && bytes.IndexByte(resp[i:], 'c') >= 0
}

func parseOSC11(resp []byte) (RGB, bool) {
	_, rest, ok := strings.Cut(string(resp), "rgb:")
	if !ok {
		return RGB{}, false
	}
	if i := strings.IndexAny(rest, "\x07\x1b"); i >= 0 {
		rest = rest[:i]
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 3 {
		return RGB{}, false
	}
	var c RGB
	for i := range c {
		h := strings.TrimSpace(parts[i])
		v, err := strconv.ParseUint(h, 16, 32)
		if err != nil || len(h) > 4 {
			return RGB{}, false
		}
		c[i] = int(v * 255 / (uint64(1)<<(4*len(h)) - 1))
	}
	return c, true
}

func (t *Terminal) queryBg(def RGB) RGB {
	out.WriteString("\x1b]11;?\x07\x1b[c")
	var resp []byte
	buf := make([]byte, 128)
	for end := time.Now().Add(bgQueryTimeout); time.Now().Before(end) && len(resp) < 256 && !daDone(resp); {
		n := t.read(buf)
		if n < 0 {
			break
		}
		if n == 0 {
			time.Sleep(2 * time.Millisecond)
		}
		resp = append(resp, buf[:n]...)
	}
	if c, ok := parseOSC11(resp); ok {
		return c
	}
	return def
}

func (t *Terminal) start() { out.WriteString("\x1b[?25l\x1b[?2004l\x1b[?7l") }

func (t *Terminal) pollKey(buf []byte) int {
	for t.keys {
		n := t.read(buf)
		if n < 0 {
			t.keys = false
			t.enableISIG()
			break
		}
		if n == 0 {
			break
		}
		for _, c := range buf[:n] {
			if code, ok := exitKeys[c]; ok {
				return code
			}
		}
	}
	return -1
}

func (t *Terminal) enableISIG() {
	var s syscall.Termios
	if ioctl(t.fd, syscall.TCGETS, &s) {
		s.Lflag |= syscall.ISIG
		ioctl(t.fd, syscall.TCSETS, &s)
	}
}

func (t *Terminal) restore() {
	if t.raw {
		ioctl(t.fd, tcsetsf, &t.orig)
	}
	out.WriteString("\x1b[?2026l\x1b[?7h\x1b[?2004h\x1b[?25h\x1b[m")
}

func (t *Terminal) abort(code, lines int) {
	if lines > 0 {
		fmt.Fprintf(out, "\x1b[%dA\r\x1b[J", lines)
	}
	t.restore()
	os.Exit(code)
}

func main() {
	if !isTTY(1) || os.Getenv("TERM") == "dumb" {
		return
	}
	r := newRenderer()
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		r.plain = true
		out.Write(r.frame(0, 0))
		return
	}
	term := &Terminal{fd: int(os.Stdin.Fd())}
	defer term.restore()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	term.enterRaw()
	if term.raw {
		r.bg = term.queryBg(titleDark)
		term.flush()
	}
	term.start()
	tick := time.NewTicker(time.Second / fps)
	defer tick.Stop()
	start := time.Now()
	keys := make([]byte, 128)
	lines := 0
	for {
		t := time.Since(start).Seconds()
		code := term.pollKey(keys)
		select {
		case s := <-sig:
			code = 128 + int(s.(syscall.Signal))
		default:
		}
		if code >= 0 {
			term.abort(code, lines)
		}
		out.Write(r.frame(t, lines))
		lines = logo.h
		if t >= baseEnd {
			return
		}
		<-tick.C
	}
}
