package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mattn/go-isatty"
	"golang.org/x/term"

	"github.com/emoss08/assay/internal/ui"
)

const (
	plainProgressInterval = 5 * time.Second

	// ttyRedrawInterval caps redraw frequency. Progress callbacks fire once per
	// completed unit, which for fast phases can be thousands per second; a
	// capped redraw keeps the bar smooth without making rendering measurable.
	ttyRedrawInterval = 50 * time.Millisecond

	progressBarWidth = 24
)

// newProgress returns a progress reporter suited to where its output lands. On
// a terminal it rewrites one line with a live bar, percentage, throughput and
// the item in flight; anywhere else it prints an occasional plain line,
// because carriage returns and erase-line escapes turn CI logs into noise.
func newProgress(out io.Writer, quiet bool, paint ui.Painter) func(item string, done, total int) {
	if quiet {
		return nil
	}

	if isTerminal(out) {
		var (
			mu    sync.Mutex
			start time.Time
			last  time.Time
		)

		return func(item string, done, total int) {
			mu.Lock()
			defer mu.Unlock()

			now := time.Now()
			if start.IsZero() {
				start = now
			}
			if done < total && now.Sub(last) < ttyRedrawInterval {
				return
			}
			last = now

			fraction := 0.0
			if total > 0 {
				fraction = float64(done) / float64(total)
			}

			elapsed := now.Sub(start).Round(time.Second)
			counts := fmt.Sprintf("%d/%d", done, total)
			percent := fmt.Sprintf("%3.0f%%", fraction*100)

			fmt.Fprint(out, "\r\033[2K"+progressLine(
				paint, terminalWidth(out), fraction, percent, counts, elapsed.String(), item))
		}
	}

	var (
		mu    sync.Mutex
		last  time.Time
		phase string
	)

	return func(item string, done, total int) {
		mu.Lock()
		defer mu.Unlock()

		// A new phase always announces itself; otherwise the first line of a short
		// phase can be swallowed by the previous phase's interval.
		if item == phase && done < total && time.Since(last) < plainProgressInterval {
			return
		}
		last = time.Now()
		phase = item
		fmt.Fprintf(out, "%s %d/%d\n", item, done, total)
	}
}

func finishProgress(out io.Writer, quiet bool) {
	if quiet || !isTerminal(out) {
		return
	}
	fmt.Fprint(out, "\r\033[2K")
}

// progressLine renders the bar, percentage, counts, elapsed time and the item
// in flight, never letting the visible line reach the terminal's right edge: a
// wrapped progress line puts the cursor on a new row, \r returns to the start
// of that row only, and every earlier row's fragment is left behind as
// garbage. The item gets whatever width remains after the fixed parts and is
// dropped entirely on tiny terminals.
func progressLine(
	paint ui.Painter,
	width int,
	fraction float64,
	percent, counts, elapsed, item string,
) string {
	fixed := progressBarWidth + 1 + len(percent) + 1 + len(counts) + 1 + len(elapsed)
	if fixed > width-1 {
		minimal := []rune(strings.TrimSpace(percent) + " " + counts)
		if len(minimal) > width-1 {
			minimal = minimal[:max(width-1, 0)]
		}

		return paint.Bold(string(minimal))
	}
	line := fmt.Sprintf("%s %s %s %s",
		paint.Bar(fraction, progressBarWidth),
		paint.Bold(percent),
		counts,
		paint.Muted(elapsed),
	)
	if room := width - 1 - fixed - 1; room >= 12 {
		line += " " + paint.Muted(shortenTail(item, room))
	}

	return line
}

// terminalWidth reports the current width of the terminal behind out,
// re-queried on every call so live resizes are respected. The query is one
// ioctl — nanoseconds against a 50ms redraw budget.
func terminalWidth(out io.Writer) int {
	file, ok := out.(*os.File)
	if !ok {
		return 80
	}
	width, _, err := term.GetSize(int(file.Fd()))
	if err != nil || width <= 0 {
		return 80
	}

	return width
}

func isTerminal(out io.Writer) bool {
	file, ok := out.(*os.File)
	if !ok {
		return false
	}
	fd := file.Fd()

	return isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd)
}
