package cli

import (
	"strings"
	"testing"

	"github.com/emoss08/assay/internal/ui"
)

func TestProgressLineNeverReachesTheTerminalEdge(t *testing.T) {
	paint := ui.New(false)
	longItem := "github.com/emoss08/trenova/internal/infrastructure/postgres/repositories/shipmentholdrepository"

	for _, width := range []int{20, 40, 60, 80, 100, 120, 200} {
		line := progressLine(paint, width, 0.42, " 42%", "151/360", "3m12s", longItem)
		if got := len(visibleRunes(line)); got > width-1 {
			t.Errorf("width %d: line occupies %d columns, must stay under %d", width, got, width)
		}
	}
}

func TestProgressLineKeepsTheItemWhenThereIsRoom(t *testing.T) {
	paint := ui.New(false)

	line := progressLine(paint, 120, 0.5, " 50%", "180/360", "1m2s", "github.com/emoss08/assay/internal/cli")
	if !strings.Contains(line, "internal/cli") {
		t.Fatalf("wide terminal must show the item, got %q", line)
	}

	narrow := progressLine(paint, 45, 0.5, " 50%", "180/360", "1m2s", "github.com/emoss08/assay/internal/cli")
	if strings.Contains(narrow, "internal/cli") {
		t.Fatalf("narrow terminal must drop the item rather than wrap, got %q", narrow)
	}
}

func visibleRunes(s string) []rune {
	var out []rune
	inEscape := false
	for _, r := range s {
		switch {
		case inEscape:
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEscape = false
			}
		case r == '\033':
			inEscape = true
		default:
			out = append(out, r)
		}
	}

	return out
}
