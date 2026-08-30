package helper

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/robertpitt/git3/internal/engine"
)

const progressRefreshInterval = 100 * time.Millisecond

type terminalProgress struct {
	out      io.Writer
	mu       sync.Mutex
	phase    string
	started  time.Time
	last     time.Time
	lineOpen bool
}

func (p *terminalProgress) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.out == nil {
		return len(b), nil
	}
	if p.lineOpen {
		if _, err := fmt.Fprintln(p.out); err != nil {
			return 0, err
		}
		p.lineOpen = false
	}
	n, err := p.out.Write(b)
	if n > 0 {
		p.lineOpen = b[n-1] != '\n'
	}
	return n, err
}

func (p *terminalProgress) Update(event engine.ProgressEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.out == nil || event.Phase == "" {
		return
	}
	now := time.Now()
	if event.Phase != p.phase {
		if p.lineOpen {
			fmt.Fprintln(p.out)
		}
		p.phase = event.Phase
		p.started = now
		p.last = time.Time{}
		p.lineOpen = false
	}
	if !event.Done && !p.last.IsZero() && event.Current < event.Total && now.Sub(p.last) < progressRefreshInterval {
		return
	}
	if event.Total == 0 {
		if event.Done {
			fmt.Fprintf(p.out, "%s: done.\n", event.Phase)
			p.lineOpen = false
		} else {
			fmt.Fprintf(p.out, "%s...\r", event.Phase)
			p.lineOpen = true
		}
		p.last = now
		return
	}
	current := event.Current
	if current > event.Total {
		current = event.Total
	}
	percent := uint64(float64(current) / float64(event.Total) * 100)
	if percent > 100 {
		percent = 100
	}
	currentText, totalText := fmt.Sprintf("%d", current), fmt.Sprintf("%d", event.Total)
	if event.Unit == engine.ProgressUnitBytes {
		currentText, totalText = formatBytes(current), formatBytes(event.Total)
	}
	rate := ""
	if event.Unit == engine.ProgressUnitBytes && current > 0 {
		elapsed := now.Sub(p.started).Seconds()
		if elapsed >= 0.01 {
			rate = ", " + formatBytes(uint64(float64(current)/elapsed)) + "/s"
		}
	}
	suffix := "\r"
	if event.Done {
		suffix = ", done.\n"
	}
	fmt.Fprintf(p.out, "%s: %3d%% (%s/%s)%s%s", event.Phase, percent, currentText, totalText, rate, suffix)
	p.lineOpen = !event.Done
	p.last = now
}

func formatBytes(value uint64) string {
	const unit = uint64(1024)
	if value < unit {
		return fmt.Sprintf("%d B", value)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	scaled := float64(value)
	index := -1
	for scaled >= float64(unit) && index < len(units)-1 {
		scaled /= float64(unit)
		index++
	}
	return fmt.Sprintf("%.1f %s", scaled, units[index])
}
