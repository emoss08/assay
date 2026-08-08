package index

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/emoss08/assay/internal/overlay"
)

// TestDecodeWindowsAgreesWithCovdataTextfmt is the contract for the in-process
// covdata decoder: for the same counter directory, decodeWindows and the
// `go tool covdata textfmt` subprocess path must produce identical blocks. It
// drives the real toolchain — a real instrumented build, a real harness run —
// so a Go release that changes the binary formats fails this test rather than
// silently corrupting records; decoding then rejects the version and every
// caller falls back to the subprocess path.
func TestDecodeWindowsAgreesWithCovdataTextfmt(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeCovdataFixture(t, root)

	workdir := t.TempDir()
	injected := filepath.Join(root, "fixture", HarnessFileName)
	harnessPath, err := overlay.WriteUnderBasename(workdir, "harness", injected, HarnessSource("fixture"))
	require.NoError(t, err)
	overlayPath, err := overlay.WriteFile(workdir, map[string]string{injected: harnessPath})
	require.NoError(t, err)

	ctx := context.Background()
	opts := Options{Root: root}

	binary := filepath.Join(workdir, "harness.bin")
	require.NoError(t, buildTestBinary(
		ctx, opts, "example.com/covfixture/...", "example.com/covfixture/fixture", binary, overlayPath, 2))

	dir := filepath.Join(root, "fixture")
	names, err := listTests(ctx, opts, binary, dir)
	require.NoError(t, err)
	require.NotEmpty(t, names)

	listFile := filepath.Join(workdir, "tests.list")
	require.NoError(t, os.WriteFile(listFile, []byte(strings.Join(names, "\n")+"\n"), 0o644))

	counterDir := filepath.Join(workdir, "counters")
	require.NoError(t, os.MkdirAll(filepath.Join(counterDir, "assay-final"), 0o755))

	run := runHarness(ctx, opts, binary, dir, listFile, counterDir, names)
	require.NoError(t, run.err)
	require.Len(t, run.completed, len(names))

	windows := make([]string, 0, len(names)+1)
	windows = append(windows, initWindowName)
	windows = append(windows, names...)

	decoded, err := decodeWindows(counterDir, windows)
	require.NoError(t, err)

	converted, err := convertWindows(ctx, opts, counterDir, windows, 2)
	require.NoError(t, err)

	require.Equal(t, converted, decoded,
		"in-process covdata decoding must match `go tool covdata textfmt` exactly")

	// The decoder must have seen real coverage, not agreed on emptiness.
	total := 0
	for _, blocks := range decoded {
		total += len(blocks)
	}
	require.Positive(t, total)
}

func TestDecodeMetaRejectsForeignPayloads(t *testing.T) {
	t.Parallel()

	_, err := decodeMeta([]byte("not a meta file at all"))
	require.Error(t, err)

	_, err = decodeMeta(nil)
	require.Error(t, err)

	// A valid magic with a future version must refuse, not guess.
	future := append([]byte{0x00, 'c', 'v', 'm'}, []byte{2, 0, 0, 0}...)
	future = append(future, make([]byte, 64)...)
	_, err = decodeMeta(future)
	require.ErrorContains(t, err, "version")
}

func TestAccumulateCountersRejectsForeignPayloads(t *testing.T) {
	t.Parallel()

	meta := &covMeta{}

	require.Error(t, meta.accumulateCounters([]byte("junk"), nil))

	// Valid magic and version but a mismatched meta hash must refuse: pairing
	// counters with a meta they were not produced against fabricates coverage.
	mismatched := append([]byte{0x00, 'c', 'w', 'm'}, []byte{1, 0, 0, 0}...)
	mismatched = append(mismatched, []byte("0123456789abcdef")...)
	mismatched = append(mismatched, make([]byte, 40)...)
	require.Error(t, meta.accumulateCounters(mismatched, nil))
}

// writeCovdataFixture lays out a module whose coverage exercises the decoder's
// interesting axes: init-time execution, multiple files in one package, a
// dependency package pulled in through -coverpkg, per-test disjoint execution,
// and a skipped test with an empty window.
func writeCovdataFixture(t *testing.T, root string) {
	t.Helper()

	files := map[string]string{
		"go.mod": "module example.com/covfixture\n\ngo 1.26\n",

		"dep/dep.go": `package dep

func Double(v int) int {
	return v * 2
}
`,
		"fixture/a.go": `package fixture

import "example.com/covfixture/dep"

var initialised = seed()

func seed() int {
	return dep.Double(21)
}

func Left(v int) int {
	if v > 0 {
		return v + initialised
	}

	return -v
}
`,
		"fixture/b.go": `package fixture

func Right(v int) int {
	out := 0
	for i := 0; i < v; i++ {
		out += i
	}

	return out
}
`,
		"fixture/fixture_test.go": `package fixture

import "testing"

func TestLeft(t *testing.T) {
	if Left(1) != 43 {
		t.Fatal("bad")
	}
}

func TestRight(t *testing.T) {
	if Right(3) != 3 {
		t.Fatal("bad")
	}
}

func TestSkipped(t *testing.T) {
	t.Skip("nothing to see")
}
`,
	}

	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
}
