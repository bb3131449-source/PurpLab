package main

import (
	"bytes"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"
	"unsafe"
)

type RGB [3]int
type Vec [3]float64

type Logo struct {
	rows [][]rune
	w, h int
}

type Cell struct {
	ch     rune
	gray   RGB
	target RGB
	start  float64
}

const (
	fps = 60

	appearDur  = 0.22
	appearLag  = 0.3
	colorDelay = 0.1
	colorDur   = 0.6

	sweepSpread = 0.65
	sweepJitter = 0.07

	glowAmount = 0.5
	glowTint   = 0.3

	titleText  = "PurpNet!"
	titleDelay = 0.3
	titleStep  = 0.06
	titleFade  = 0.75
	titleMid   = 0.5
	titleGap   = 2
	titleRow   = 0

	subRow    = 1
	subIndent = titleGap + 1
	subDelay  = 0.6
	subStep   = 0.025
	subFade   = 0.6

	settle = 0.12

	bgQueryTimeout = 180 * time.Millisecond

	ioctlGet   = 0x5401
	ioctlSet   = 0x5402
	ioctlSetFl = 0x5404
	ioctlFlush = 0x540B
)

var (
	logo = newLogo([]string{
		" ▐▛██▜▌",
		"▝▜████▛▘",
		" ▐▌▐▌▐▌",
	})

	white = RGB{255, 255, 255}

	gray  = []RGB{{225, 225, 225}, {140, 140, 140}}
	color = []RGB{{163, 140, 255}, {61, 29, 191}}

	glowColor RGB

	titleDark  = RGB{14, 12, 20}
	titleRunes = []rune(titleText)

	subRunes  []rune
	subTarget []RGB

	grid [][]Cell
	lin  [256]float64
	buf  []byte

	unlockAt float64

	stdinFd = int(os.Stdin.Fd())

	out     = os.Stdout
	mu      sync.Mutex
	printed bool

	origTerm syscall.Termios
	rawOn    atomic.Bool

	readerStop  chan struct{}
	readerDone  chan struct{}
	stopOnce    sync.Once
	unlockOnce  sync.Once
	restoreOnce sync.Once
	unlocked    bool
)

func newLogo(src []string) Logo {
	l := Logo{h: len(src)}
	for _, s := range src {
		rs := []rune(s)
		l.rows = append(l.rows, rs)
		if len(rs) > l.w {
			l.w = len(rs)
		}
	}
	return l
}

func init() {
	for i := range lin {
		lin[i] = math.Pow(float64(i)/255.0, 2.2)
	}

	glowColor = mix(white, color[0], glowTint)

	grid = make([][]Cell, logo.h)
	for r, row := range logo.rows {
		grid[r] = make([]Cell, logo.w)
		for x := 0; x < logo.w; x++ {
			ch := ' '
			if x < len(row) {
				ch = row[x]
			}
			c := Cell{ch: ch}
			if ch != ' ' {
				c.gray = grad(gray, x, r)
				c.target = grad(color, x, r)
				c.start = sweepT(x, r)*sweepSpread + sweepJitter*jitter(x, r)
			}
			grid[r][x] = c
		}
	}

	subRunes = []rune(readVersion() + " • " + runtime.Version())
	subTarget = make([]RGB, len(subRunes))
	for i, ch := range subRunes {
		if ch == '•' {
			subTarget[i] = color[0]
		} else {
			subTarget[i] = gray[1]
		}
	}

	logoEnd := colorDelay + sweepSpread + sweepJitter + colorDur
	titleEnd := titleDelay + float64(len(titleRunes)-1)*titleStep + titleFade
	subEnd := subDelay + float64(len(subRunes)-1)*subStep + subFade
	unlockAt = math.Max(math.Max(logoEnd, titleEnd), subEnd) + settle
}

func readVersion() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "unknown"
	}
	data, err := os.ReadFile(filepath.Join(home, "PurpNet", ".version"))
	if err != nil {
		return "unknown"
	}
	v := strings.TrimSpace(string(data))
	if v == "" {
		return "unknown"
	}
	return v
}

func jitter(x, r int) float64 {
	v := math.Sin(float64(x)*12.9898+float64(r)*78.233) * 43758.5453
	return v - math.Floor(v)
}

func queryBgColor() RGB {
	if !rawOn.Load() {
		return titleDark
	}

	out.WriteString("\x1b]11;?\x07\x1b[c")

	deadline := time.Now().Add(bgQueryTimeout)
	rbuf := make([]byte, 128)
	var resp []byte

	for time.Now().Before(deadline) && len(resp) < 256 {
		n, err := syscall.Read(stdinFd, rbuf)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			if err == syscall.EAGAIN {
				time.Sleep(2 * time.Millisecond)
				continue
			}
			break
		}
		if n > 0 {
			resp = append(resp, rbuf[:n]...)
			if daDone(resp) {
				break
			}
		}
	}

	if c, ok := parseOSC11(resp); ok {
		return c
	}
	return titleDark
}

func daDone(resp []byte) bool {
	i := bytes.LastIndex(resp, []byte("\x1b[?"))
	return i >= 0 && bytes.IndexByte(resp[i:], 'c') >= 0
}

func parseOSC11(resp []byte) (RGB, bool) {
	idx := bytes.Index(resp, []byte("rgb:"))
	if idx < 0 {
		return RGB{}, false
	}

	rest := resp[idx+4:]
	end := bytes.IndexAny(rest, "\x07\x1b")
	if end < 0 {
		end = len(rest)
	}

	parts := strings.Split(string(rest[:end]), "/")
	if len(parts) < 3 {
		return RGB{}, false
	}

	var c RGB
	for i := 0; i < 3; i++ {
		h := strings.TrimSpace(parts[i])
		if h == "" || len(h) > 4 {
			return RGB{}, false
		}
		v, err := strconv.ParseUint(h, 16, 32)
		if err != nil {
			return RGB{}, false
		}
		var maxV uint64
		switch len(h) {
		case 1:
			maxV = 0xF
		case 2:
			maxV = 0xFF
		case 3:
			maxV = 0xFFF
		case 4:
			maxV = 0xFFFF
		}
		c[i] = int(v * 255 / maxV)
	}
	return c, true
}

func clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }

func smootherstep(x float64) float64 {
	x = clamp01(x)
	return x * x * x * (x*(x*6-15) + 10)
}

func toVec(c RGB) Vec { return Vec{lin[c[0]], lin[c[1]], lin[c[2]]} }

func toRGB(v Vec) RGB {
	var o RGB
	for i := range o {
		o[i] = int(math.Round(255.0 * math.Pow(clamp01(v[i]), 1.0/2.2)))
	}
	return o
}

func mix(a, b RGB, k float64) RGB {
	if k <= 0 {
		return a
	}
	if k >= 1 {
		return b
	}
	va, vb := toVec(a), toVec(b)
	var v Vec
	for i := range v {
		v[i] = va[i] + (vb[i]-va[i])*k
	}
	return toRGB(v)
}

func stopsAt(stops []RGB, t float64) RGB {
	t = clamp01(t) * float64(len(stops)-1)
	i := int(t)
	if i > len(stops)-2 {
		i = len(stops) - 2
	}
	return mix(stops[i], stops[i+1], t-float64(i))
}

func sweepT(x, r int) float64 {
	wd := float64(logo.w - 1)
	if wd < 1 {
		wd = 1
	}
	hd := float64(logo.h - 1)
	if hd < 1 {
		hd = 1
	}
	return clamp01((float64(x)/wd + float64(r)/hd) / 2)
}

func gradT(x, r int) float64 {
	wd := float64(logo.w - 1)
	if wd < 1 {
		wd = 1
	}
	hd := float64(logo.h - 1)
	if hd < 1 {
		hd = 1
	}
	return clamp01(0.8*float64(x)/wd + 0.2*float64(r)/hd)
}

func grad(stops []RGB, x, r int) RGB {
	return stopsAt(stops, gradT(x, r))
}

func cellColor(c *Cell, t float64) (RGB, bool) {
	k := smootherstep((t - c.start*appearLag) / appearDur)
	if k <= 0 {
		return RGB{}, false
	}
	s := smootherstep((t - colorDelay - c.start) / colorDur)
	if s >= 1 {
		return c.target, true
	}
	o := mix(mix(titleDark, c.gray, k), c.target, s)
	if g := math.Sin(math.Pi * s); g > 0 {
		o = mix(o, glowColor, glowAmount*g)
	}
	return o, true
}

func titleColor(i int, t float64) (RGB, bool) {
	u := (t - titleDelay - float64(i)*titleStep) / titleFade
	if u <= 0 {
		return RGB{}, false
	}
	k := smootherstep(u)
	if k >= 1 {
		return white, true
	}
	if k < titleMid {
		return mix(titleDark, color[0], k/titleMid), true
	}
	return mix(color[0], white, (k-titleMid)/(1-titleMid)), true
}

func subColor(i int, t float64) (RGB, bool) {
	u := (t - subDelay - float64(i)*subStep) / subFade
	if u <= 0 {
		return RGB{}, false
	}
	return mix(titleDark, subTarget[i], smootherstep(u)), true
}

func appendFG(b []byte, c RGB) []byte {
	b = append(b, "\x1b[38;2;"...)
	b = strconv.AppendInt(b, int64(c[0]), 10)
	b = append(b, ';')
	b = strconv.AppendInt(b, int64(c[1]), 10)
	b = append(b, ';')
	b = strconv.AppendInt(b, int64(c[2]), 10)
	return append(b, 'm')
}

func frame(t float64) []byte {
	b := buf[:0]
	b = append(b, "\x1b[?2026h"...)
	if printed {
		b = append(b, "\x1b["...)
		b = strconv.AppendInt(b, int64(logo.h), 10)
		b = append(b, "A\r"...)
	}

	for r := 0; r < logo.h; r++ {
		var last RGB
		have := false

		for i := range grid[r] {
			c := &grid[r][i]
			if c.ch == ' ' {
				b = append(b, ' ')
				continue
			}
			col, ok := cellColor(c, t)
			if !ok {
				b = append(b, ' ')
				continue
			}
			if !have || col != last {
				b = appendFG(b, col)
				last, have = col, true
			}
			b = utf8.AppendRune(b, c.ch)
		}
		b = append(b, "\x1b[m"...)

		if r == titleRow {
			for g := 0; g < titleGap; g++ {
				b = append(b, ' ')
			}
			have = false
			for i, ch := range titleRunes {
				col, ok := titleColor(i, t)
				if !ok {
					b = append(b, ' ')
					continue
				}
				if !have || col != last {
					b = appendFG(b, col)
					last, have = col, true
				}
				b = utf8.AppendRune(b, ch)
			}
			b = append(b, "\x1b[m"...)
		}

		if r == subRow {
			for g := 0; g < subIndent; g++ {
				b = append(b, ' ')
			}
			have = false
			for i, ch := range subRunes {
				col, ok := subColor(i, t)
				if !ok {
					b = append(b, ' ')
					continue
				}
				if !have || col != last {
					b = appendFG(b, col)
					last, have = col, true
				}
				b = utf8.AppendRune(b, ch)
			}
			b = append(b, "\x1b[m"...)
		}

		b = append(b, "\x1b[K\n"...)
	}

	b = append(b, "\x1b[?2026l"...)
	buf = b
	return b
}

func plainFrame() []byte {
	var b []byte
	for r := 0; r < logo.h; r++ {
		b = append(b, string(logo.rows[r])...)
		if r == titleRow {
			b = append(b, strings.Repeat(" ", titleGap)...)
			b = append(b, titleText...)
		}
		if r == subRow {
			b = append(b, strings.Repeat(" ", subIndent)...)
			b = append(b, string(subRunes)...)
		}
		b = append(b, '\n')
	}
	return b
}

func ioctl(fd int, req uintptr, t *syscall.Termios) bool {
	_, _, e := syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(fd),
		req,
		uintptr(unsafe.Pointer(t)),
	)
	return e == 0
}

func isTTY(fd int) bool {
	var t syscall.Termios
	return ioctl(fd, ioctlGet, &t)
}

func flushInput() {
	if rawOn.Load() {
		syscall.Syscall(
			syscall.SYS_IOCTL,
			uintptr(stdinFd),
			uintptr(ioctlFlush),
			0,
		)
	}
}

func enableISIG() {
	var t syscall.Termios
	if ioctl(stdinFd, ioctlGet, &t) {
		t.Lflag |= syscall.ISIG
		ioctl(stdinFd, ioctlSet, &t)
	}
}

func restoreTerminal() {
	restoreOnce.Do(func() {
		if rawOn.Load() {
			t := origTerm
			ioctl(stdinFd, ioctlSetFl, &t)
			rawOn.Store(false)
		}
		out.WriteString("\x1b[?2026l\x1b[?7h\x1b[?2004h\x1b[?25h\x1b[m")
	})
}

func enableRaw() {
	if rawOn.Load() {
		return
	}
	var t syscall.Termios
	if !ioctl(stdinFd, ioctlGet, &t) {
		return
	}
	origTerm = t
	t.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	t.Iflag &^= syscall.IXON | syscall.ICRNL
	t.Cc[syscall.VMIN] = 0
	t.Cc[syscall.VTIME] = 1
	if ioctl(stdinFd, ioctlSet, &t) {
		rawOn.Store(true)
		flushInput()
	}
}

func lockInput() {
	out.WriteString("\x1b[?25l\x1b[?2004l\x1b[?7l")
	enableRaw()
	if rawOn.Load() {
		readerStop = make(chan struct{})
		readerDone = make(chan struct{})
		go readKeys(readerStop, readerDone)
	}
}

func unlockInput() {
	unlockOnce.Do(func() {
		if readerStop != nil {
			close(readerStop)
		}
		if readerDone != nil {
			<-readerDone
		}
		flushInput()
		restoreTerminal()

		mu.Lock()
		unlocked = true
		mu.Unlock()
	})
}

func stopSoft() {
	stopOnce.Do(func() {
		mu.Lock()
		if printed && !unlocked {
			out.WriteString("\x1b[" + strconv.Itoa(logo.h) + "A\r\x1b[J")
		}
		mu.Unlock()

		flushInput()
		restoreTerminal()
		os.Exit(0)
	})
}

func readKeys(stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	rbuf := make([]byte, 128)
	for {
		select {
		case <-stop:
			return
		default:
		}
		n, err := syscall.Read(stdinFd, rbuf)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			if err == syscall.EAGAIN {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			enableISIG()
			return
		}
		if n <= 0 {
			continue
		}
		for _, c := range rbuf[:n] {
			if c == 3 || c == 4 || c == 28 {
				stopSoft()
				return
			}
		}
	}
}

func main() {
	if !isTTY(1) || os.Getenv("TERM") == "dumb" {
		return
	}
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		out.Write(plainFrame())
		return
	}

	defer restoreTerminal()

	enableRaw()
	if rawOn.Load() {
		titleDark = queryBgColor()
		flushInput()
	}

	lockInput()

	sig := make(chan os.Signal, 1)
	sigDone := make(chan struct{})

	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)

	go func() {
		select {
		case <-sig:
			stopSoft()
		case <-sigDone:
			return
		}
	}()

	ticker := time.NewTicker(time.Second / fps)
	defer ticker.Stop()

	start := time.Now()

	for {
		t := time.Since(start).Seconds()
		done := t >= unlockAt

		if done {
			t = 99
		}

		mu.Lock()
		out.Write(frame(t))
		printed = true
		mu.Unlock()

		if done {
			unlockInput()

			signal.Stop(sig)

			select {
			case <-sig:
			default:
			}

			close(sigDone)
			break
		}

		<-ticker.C
	}
}
