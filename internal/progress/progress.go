package progress

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

const (
	barWidth    = 50
	refreshRate = 100 * time.Millisecond
)

type Bar struct {
	total     int64
	current   atomic.Int64
	startTime time.Time
	done      chan struct{}
	output    io.Writer
}

func NewBar(total int64, showProgress bool) *Bar {
	if !showProgress {
		return nil
	}

	// Always use stderr when stdout is being used for data
	output := io.Writer(os.Stdout)
	if isStdoutPiped() {
		output = os.Stderr
	}

	bar := &Bar{
		total:     total,
		startTime: time.Now(),
		done:      make(chan struct{}),
		output:    output,
	}

	go bar.render()
	return bar
}

func isStdoutPiped() bool {
	stat, _ := os.Stdout.Stat()
	return (stat.Mode() & os.ModeCharDevice) == 0
}

func (b *Bar) Add(n int64) {
	if n > 0 {
		b.current.Add(n)
	}
}

func (b *Bar) Finish() {
	close(b.done)
}

func (b *Bar) render() {
	ticker := time.NewTicker(refreshRate)
	defer ticker.Stop()

	for {
		select {
		case <-b.done:
			b.draw()
			fmt.Fprintln(b.output)
			return
		case <-ticker.C:
			b.draw()
		}
	}
}

func (b *Bar) draw() {
	current := b.current.Load()
	if current > b.total {
		current = b.total
	}

	percent := float64(current) / float64(b.total) * 100
	filled := int(float64(barWidth) * float64(current) / float64(b.total))

	// Ensure filled is within bounds
	if filled < 0 {
		filled = 0
	}
	if filled > barWidth {
		filled = barWidth
	}

	bar := strings.Repeat("=", filled) + strings.Repeat(" ", barWidth-filled)

	elapsed := time.Since(b.startTime).Seconds()
	var speed float64
	if elapsed > 0 {
		speed = float64(current) / elapsed / 1024 / 1024 // MB/s
	}

	fmt.Fprintf(b.output, "\r[%s] %.1f%% %.1f MB/s", bar, percent, speed)
}
