package main

import (
	"fmt"
	"math"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

type RGB [3]int

type Logo struct {
	rows [][]rune
	w, h int
}

const (
	trans   = 1.6
	logoEnd = trans
	fps     = 30
	gap     = 2

	sweepSpread = 0.6
	sweepFront  = 0.4
	flash       = 0.55

	subDelay = 0.1
	subFade  = 0.4
	subStep  = 0.12

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

	subC    = RGB{150, 156, 166}
	subFrom = RGB{50, 52, 58}

	subs = map[int]string{}

	animEnd  float64
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
	animEnd = logoEnd + 0.2

	for r := range subs {
		e := logoEnd + subDelay + float64(r)*subStep + subFade
		if e > animEnd {
			animEnd = e
		}
	}

	unlockAt = animEnd
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }
func clamp01(v float64) float64       { return clamp(v, 0, 1) }
func smooth(s float64) float64        { return s * s * (3 - 2*s) }

func mix(a, b RGB, k float64) RGB {
	var o RGB
	for i := range o {
		o[i] = int(math.Round(float64(a[i]) + float64(b[i]-a[i])*k))
	}
	return o
}

func stopsAt(stops []RGB, t float64) RGB {
	t = clamp01(t) * float64(len(stops)-1)
	i := int(t)
	if i > len(stops)-2 {
		i = len(stops) - 2
	}
	return mix(stops[i], stops[i+1], t-float64(i))
}

func diagT(x, r int) float64 {
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

func grad(stops []RGB, x, r int) RGB {
	return stopsAt(stops, smooth(diagT(x, r)))
}

func tint(g, target RGB, x, r int, t float64) RGB {
	u := t / trans
	s := clamp01((u - diagT(x, r)*sweepSpread) / sweepFront)
	k := smooth(s)
	glow := math.Sin(math.Pi * s)

	o := mix(g, target, k)
	return mix(o, white, clamp01(flash*glow))
}

func logoRows(t float64) []string {
	rows := make([]string, logo.h)

	for r, row := range logo.rows {
		var b strings.Builder

		for x := 0; x < logo.w; x++ {
			ch := ' '
			if x < len(row) {
				ch = row[x]
			}

			if ch == ' ' {
				b.WriteByte(' ')
				continue
			}

			fg := tint(grad(gray, x, r), grad(color, x, r), x, r, t)
			fmt.Fprintf(&b, "\x1b[38;2;%d;%d;%dm%c", fg[0], fg[1], fg[2], ch)
		}

		b.WriteString("\x1b[m")
		rows[r] = b.String()
	}

	return rows
}

func subPart(r int, t float64) string {
	s, ok := subs[r]
	if !ok {
		return ""
	}

	k := clamp01((t - logoEnd - subDelay - float64(r)*subStep) / subFade)
	if k <= 0 {
		return ""
	}

	c := mix(subFrom, subC, smooth(k))
	return fmt.Sprintf("\x1b[38;2;%d;%d;%dm%s\x1b[m", c[0], c[1], c[2], s)
}

func frame(t float64) string {
	rows := logoRows(t)
	pad := strings.Repeat(" ", gap)

	var b strings.Builder

	for r := 0; r < logo.h; r++ {
		b.WriteString(rows[r])

		if s := subPart(r, t); s != "" {
			b.WriteString(pad)
			b.WriteString(s)
		}

		b.WriteString("\x1b[K\n")
	}

	return b.String()
}

func ioctl(req uintptr, t *syscall.Termios) bool {
	_, _, e := syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(stdinFd),
		req,
		uintptr(unsafe.Pointer(t)),
	)
	return e == 0
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
	if ioctl(ioctlGet, &t) {
		t.Lflag |= syscall.ISIG
		ioctl(ioctlSet, &t)
	}
}

func restoreTerminal() {
	restoreOnce.Do(func() {
		if rawOn.Load() {
			t := origTerm
			ioctl(ioctlSetFl, &t)
			rawOn.Store(false)
		}

		out.WriteString("\x1b[?7h\x1b[?2004h\x1b[?25h\x1b[m")
	})
}

func enableRaw() {
	var t syscall.Termios
	if !ioctl(ioctlGet, &t) {
		return
	}

	origTerm = t

	t.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	t.Iflag &^= syscall.IXON | syscall.ICRNL

	t.Cc[syscall.VMIN] = 0
	t.Cc[syscall.VTIME] = 1

	if ioctl(ioctlSet, &t) {
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

func stop_soft() {
	stopOnce.Do(func() {
		mu.Lock()
		if printed && !unlocked {
			fmt.Fprintf(out, "\x1b[%dA\r\x1b[J", logo.h)
		}
		mu.Unlock()

		flushInput()
		restoreTerminal()
		os.Exit(0)
	})
}

func readKeys(stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)

	buf := make([]byte, 128)

	for {
		select {
		case <-stop:
			return
		default:
		}

		n, err := syscall.Read(stdinFd, buf)
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

		for _, c := range buf[:n] {
			if c == 3 || c == 4 || c == 28 {
				stop_soft()
				return
			}
		}
	}
}

func main() {
	lockInput()
	defer restoreTerminal()

	sig := make(chan os.Signal, 1)
	sigDone := make(chan struct{})

	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)

	go func() {
		select {
		case <-sig:
			stop_soft()
		case <-sigDone:
			return
		}
	}()

	start := time.Now()

	for {
		t := time.Since(start).Seconds()
		done := t >= unlockAt

		if done {
			t = 99
		}

		mu.Lock()
		if printed {
			fmt.Fprintf(out, "\x1b[%dA\r", logo.h)
		}
		out.WriteString(frame(t))
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

		time.Sleep(time.Second / fps)
	}
}
