package app

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/x/term"
	"github.com/mitsuhiko/gh-issue-sync/internal/ghcli"
	"github.com/mitsuhiko/gh-issue-sync/internal/termcolor"
	"github.com/mitsuhiko/gh-issue-sync/internal/theme"
)

type progressReporter struct {
	out         io.Writer
	file        *os.File
	isTTY       bool
	started     bool
	theme       *theme.Theme
	start       time.Time
	lastEvent   ghcli.ProgressEvent
	cursorHidden bool
	cursorUnreg func()
	pulseActive bool
	pulsePos    int
	pulseDir    int
	pulseStop   chan struct{}
	pulseDone   chan struct{}
	mu          sync.Mutex
}

const (
	progressBarWidth = 32
	pulseWidth       = 6
	pulseInterval    = 80 * time.Millisecond
)

func newProgressReporter(out io.Writer, t *theme.Theme) *progressReporter {
	isTTY := false
	var file *os.File
	if f, ok := out.(*os.File); ok {
		file = f
		isTTY = term.IsTerminal(f.Fd())
	}
	if t == nil {
		t = theme.Default()
	}
	return &progressReporter{
		out:      out,
		file:     file,
		isTTY:    isTTY,
		theme:    t,
		pulseDir: 1,
	}
}

func (p *progressReporter) Update(event ghcli.ProgressEvent) {
	if !p.isTTY {
		if event.Stage == ghcli.ProgressListIssuesPageStart {
			if !p.started {
				fmt.Fprintln(p.out, "Fetching issues from GitHub...")
				p.started = true
			}
			return
		}
		if event.Stage == ghcli.ProgressListIssuesPageDone {
			fmt.Fprintf(p.out, "Fetched %d issues (page %d)\n", event.Issues, event.Page)
			p.started = true
		}
		return
	}

	p.mu.Lock()
	p.hideCursorLocked()
	if p.start.IsZero() {
		p.start = time.Now()
	}
	p.lastEvent = event
	if event.Total > 0 {
		p.pulseActive = false
	} else {
		p.startPulseLocked()
	}
	msg := p.formatProgressLineLocked(event)
	fmt.Fprintf(p.out, "\r%s\x1b[K", msg)
	p.started = true
	p.mu.Unlock()
}

func (p *progressReporter) Done() {
	if !p.isTTY {
		return
	}
	p.stopPulse()
	p.mu.Lock()
	if p.started {
		fmt.Fprint(p.out, "\r\x1b[K")
	}
	p.mu.Unlock()
	p.restoreCursor()
	p.unregisterCursorRestore()
}

func (p *progressReporter) formatProgressLineLocked(event ghcli.ProgressEvent) string {
	label := p.formatLeftTextLocked(event)
	bar := p.renderBarLocked(event)
	eta := padLeftVisible(p.formatETALocked(event), 7)
	right := fmt.Sprintf("%s %s", bar, eta)
	width := p.lineWidthLocked()
	if width <= 0 {
		return fmt.Sprintf("%s %s", label, right)
	}
	visibleLeft := visibleLen(label)
	visibleRight := visibleLen(right)
	if visibleRight+1 >= width {
		return right
	}
	maxLeft := width - visibleRight - 1
	if visibleLeft > maxLeft {
		label = truncateVisible(label, maxLeft, p.theme.Styler().Reset())
		visibleLeft = visibleLen(label)
	}
	padding := width - visibleLeft - visibleRight
	return label + strings.Repeat(" ", padding) + right
}

func (p *progressReporter) formatLeftTextLocked(event ghcli.ProgressEvent) string {
	if event.Total > 0 {
		percent := int(float64(event.Issues) / float64(event.Total) * 100)
		if percent < 0 {
			percent = 0
		} else if percent > 100 {
			percent = 100
		}
		return fmt.Sprintf(
			"%s  %2d%%",
			p.theme.MutedText("Fetching issues"),
			percent,
		)
	}
	if event.Issues > 0 || event.Page > 0 {
		return fmt.Sprintf(
			"%s (%d fetched, page %d)",
			p.theme.MutedText("Fetching issues"),
			event.Issues,
			event.Page,
		)
	}
	return p.theme.MutedText("Fetching issues")
}

func (p *progressReporter) renderBarLocked(event ghcli.ProgressEvent) string {
	if event.Total <= 0 {
		return p.renderIndeterminateBarLocked()
	}
	progress := float64(event.Issues) / float64(event.Total)
	if progress < 0 {
		progress = 0
	} else if progress > 1 {
		progress = 1
	}
	filled := int(progress * float64(progressBarWidth))
	if filled > progressBarWidth {
		filled = progressBarWidth
	}
	return p.renderDeterminateBarLocked(filled)
}

func (p *progressReporter) renderDeterminateBarLocked(filled int) string {
	if filled < 0 {
		filled = 0
	} else if filled > progressBarWidth {
		filled = progressBarWidth
	}
	styler := p.theme.Styler()
	empty := strings.Repeat("░", progressBarWidth-filled)
	if filled == 0 {
		return styler.Fg(p.theme.Dim, empty)
	}
	if !styler.IsTruecolor() {
		return styler.Fg(p.theme.Accent, strings.Repeat("█", filled)) + styler.Fg(p.theme.Dim, empty)
	}
	var b strings.Builder
	b.Grow(progressBarWidth * 4)
	for i := 0; i < filled; i++ {
		t := 0.0
		if filled > 1 {
			t = float64(i) / float64(filled-1)
		}
		c := blendColor(p.theme.Accent, p.theme.StatusChar, t)
		b.WriteString(styler.Fg(c, "█"))
	}
	if empty != "" {
		b.WriteString(styler.Fg(p.theme.Dim, empty))
	}
	return b.String()
}

func (p *progressReporter) renderIndeterminateBarLocked() string {
	styler := p.theme.Styler()
	start := p.pulsePos
	if start < 0 {
		start = 0
	}
	maxStart := progressBarWidth - pulseWidth
	if maxStart < 0 {
		maxStart = 0
	}
	if start > maxStart {
		start = maxStart
	}
	var b strings.Builder
	b.Grow(progressBarWidth * 4)
	for i := 0; i < progressBarWidth; i++ {
		if i >= start && i < start+pulseWidth {
			if styler.IsTruecolor() {
				t := float64(i-start) / float64(max(1, pulseWidth-1))
				c := blendColor(p.theme.Accent, p.theme.StatusChar, t)
				b.WriteString(styler.Fg(c, "█"))
			} else {
				b.WriteString(styler.Fg(p.theme.Accent, "█"))
			}
			continue
		}
		b.WriteString(styler.Fg(p.theme.Dim, "░"))
	}
	return b.String()
}

func (p *progressReporter) formatETALocked(event ghcli.ProgressEvent) string {
	eta := "--:--"
	if event.Total > 0 && event.Issues > 0 && !p.start.IsZero() {
		elapsed := time.Since(p.start).Seconds()
		if elapsed > 0 {
			remaining := event.Total - event.Issues
			if remaining < 0 {
				remaining = 0
			}
			rate := float64(event.Issues) / elapsed
			if rate > 0 {
				seconds := int(float64(remaining) / rate)
				eta = formatDuration(seconds)
			}
		}
	}
	return p.theme.MutedText(eta)
}

func (p *progressReporter) startPulseLocked() {
	if p.pulseActive {
		return
	}
	p.pulseActive = true
	p.pulsePos = 0
	p.pulseDir = 1
	if p.pulseStop != nil {
		return
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	p.pulseStop = stop
	p.pulseDone = done
	go p.runPulse(stop, done)
}

func (p *progressReporter) stopPulse() {
	p.mu.Lock()
	if p.pulseStop == nil {
		p.pulseActive = false
		p.mu.Unlock()
		return
	}
	stop := p.pulseStop
	done := p.pulseDone
	p.pulseActive = false
	p.mu.Unlock()
	close(stop)
	if done != nil {
		<-done
	}
	p.mu.Lock()
	p.pulseStop = nil
	p.pulseDone = nil
	p.mu.Unlock()
}

func (p *progressReporter) runPulse(stop <-chan struct{}, done chan<- struct{}) {
	ticker := time.NewTicker(pulseInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			close(done)
			return
		case <-ticker.C:
			p.mu.Lock()
			if !p.pulseActive || !p.isTTY {
				p.mu.Unlock()
				continue
			}
			p.advancePulseLocked()
			event := p.lastEvent
			msg := p.formatProgressLineLocked(event)
			fmt.Fprintf(p.out, "\r%s\x1b[K", msg)
			p.started = true
			p.mu.Unlock()
		}
	}
}

func (p *progressReporter) hideCursorLocked() {
	if p.cursorHidden {
		return
	}
	fmt.Fprint(p.out, "\x1b[?25l")
	p.cursorHidden = true
	if p.cursorUnreg == nil {
		p.cursorUnreg = registerCursorRestore(p.restoreCursor)
	}
}

func (p *progressReporter) restoreCursor() {
	p.mu.Lock()
	if p.cursorHidden {
		fmt.Fprint(p.out, "\x1b[?25h")
		p.cursorHidden = false
	}
	p.mu.Unlock()
}

func (p *progressReporter) unregisterCursorRestore() {
	p.mu.Lock()
	unreg := p.cursorUnreg
	p.cursorUnreg = nil
	p.mu.Unlock()
	if unreg != nil {
		unreg()
	}
}

func (p *progressReporter) advancePulseLocked() {
	maxStart := progressBarWidth - pulseWidth
	if maxStart < 0 {
		maxStart = 0
	}
	next := p.pulsePos + p.pulseDir
	if next <= 0 {
		next = 0
		p.pulseDir = 1
	} else if next >= maxStart {
		next = maxStart
		p.pulseDir = -1
	}
	p.pulsePos = next
}

func (p *progressReporter) lineWidthLocked() int {
	if p.file == nil {
		return 0
	}
	width, _, err := term.GetSize(p.file.Fd())
	if err != nil {
		return 0
	}
	// Reserve one column to avoid terminal wrapping issues when writing
	// to the last column. Some terminals wrap or truncate the last char.
	if width > 0 {
		width--
	}
	return width
}

var progressAnsiPattern = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func visibleLen(s string) int {
	stripped := progressAnsiPattern.ReplaceAllString(s, "")
	return len([]rune(stripped))
}

func padLeftVisible(s string, width int) string {
	visible := visibleLen(s)
	if visible >= width {
		return s
	}
	return strings.Repeat(" ", width-visible) + s
}

var cursorRestoreOnce sync.Once
var cursorRestoreMu sync.Mutex
var cursorRestorers []func()

func registerCursorRestore(fn func()) func() {
	cursorRestoreOnce.Do(func() {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-sigs
			cursorRestoreMu.Lock()
			restorers := append([]func(){}, cursorRestorers...)
			cursorRestoreMu.Unlock()
			for _, r := range restorers {
				if r != nil {
					r()
				}
			}
			os.Exit(130)
		}()
	})
	cursorRestoreMu.Lock()
	cursorRestorers = append(cursorRestorers, fn)
	idx := len(cursorRestorers) - 1
	cursorRestoreMu.Unlock()
	return func() {
		cursorRestoreMu.Lock()
		if idx >= 0 && idx < len(cursorRestorers) {
			cursorRestorers[idx] = nil
		}
		cursorRestoreMu.Unlock()
	}
}

func truncateVisible(s string, max int, reset string) string {
	if max <= 0 {
		return ""
	}
	var b strings.Builder
	visible := 0
	for i := 0; i < len(s); {
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] != 'm' {
				j++
			}
			if j < len(s) {
				j++
			}
			b.WriteString(s[i:j])
			i = j
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			size = 1
		}
		if visible+1 > max {
			if reset != "" {
				b.WriteString(reset)
			}
			return b.String()
		}
		b.WriteRune(r)
		visible++
		i += size
	}
	return b.String()
}

func blendColor(a, b termcolor.Color, t float64) termcolor.Color {
	if t < 0 {
		t = 0
	} else if t > 1 {
		t = 1
	}
	r := uint8(float64(a.R) + (float64(b.R)-float64(a.R))*t)
	g := uint8(float64(a.G) + (float64(b.G)-float64(a.G))*t)
	bl := uint8(float64(a.B) + (float64(b.B)-float64(a.B))*t)
	return termcolor.RGB(r, g, bl)
}

func formatDuration(seconds int) string {
	if seconds < 0 {
		seconds = 0
	}
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	h := seconds / 3600
	m := (seconds % 3600) / 60
	s := seconds % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
