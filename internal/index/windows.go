package index

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sourcegraph/conc/pool"

	"github.com/emoss08/assay/internal/cover"
)

// assembleWindows converts the completed counter windows to executed source
// blocks and builds the record from them.
//
// The fast path decodes the binary covmeta/covcounters files in this process:
// the meta is read once per package and each window costs one small counter
// file, instead of one `go tool covdata textfmt` subprocess per test each
// re-reading the meta and serialising a mostly-zero text profile. Any decoding
// surprise falls back to the subprocess path wholesale — the two produce
// identical blocks, so the fallback costs time, never correctness.
//
// The init window — everything the package executed before the first test — is
// merged into every test's ranges, and merged *before* the empty-ranges check
// that marks a test always-run. That ordering is parity, not taste: a
// per-process profile always contained init execution, so a skipped test in a
// package with init-time statements had non-empty ranges and was attributable.
// Merging after the check would silently reclassify such tests.
func assembleWindows(
	ctx context.Context,
	opts Options,
	importPath, counterDir string,
	completed []harnessProgress,
	buildParallelism int,
) (*Record, error) {
	windows := make([]string, 0, len(completed)+1)
	windows = append(windows, initWindowName)
	for _, progress := range completed {
		windows = append(windows, progress.name)
	}

	blocks, err := decodeWindows(counterDir, windows)
	if err != nil {
		blocks, err = convertWindows(ctx, opts, counterDir, windows, buildParallelism)
		if err != nil {
			return nil, err
		}
	}

	record := &Record{Version: recordVersion, Package: importPath, IndexedAt: opts.Commit}
	build := newBuilder()
	resolve := profileResolver(opts.Graph)

	degrade := func(unresolved string) {
		if unresolved != "" && record.Degraded == "" {
			record.Degraded = "unresolved coverage path: " + unresolved
		}
	}

	initRanges, unresolved := blocksToRanges(blocks[0], build, resolve)
	degrade(unresolved)

	for i, progress := range completed {
		ranges, unresolvedTest := blocksToRanges(blocks[i+1], build, resolve)
		degrade(unresolvedTest)

		ranges = append(ranges, initRanges...)

		test := TestCoverage{Name: progress.name, Ranges: ranges, Duration: progress.duration}
		if len(ranges) == 0 {
			test.AlwaysRun = true
			test.Note = "no executed statements attributed; the test may have skipped"
		}
		record.Tests = append(record.Tests, test)
	}

	record.Files = build.order

	return record, nil
}

// decodeWindows is the in-process path: one meta decode, then one counter-file
// decode per window. Windows are independent and the meta is read-only, so a
// package with thousands of tests still assembles quickly; the work per window
// is proportional to what the test executed, not to the closure.
func decodeWindows(counterDir string, windows []string) ([][]cover.FileBlocks, error) {
	meta, err := readMetaDir(filepath.Join(counterDir, metaDirName))
	if err != nil {
		return nil, err
	}

	out := make([][]cover.FileBlocks, len(windows))
	for i, window := range windows {
		blocks, decodeErr := meta.windowBlocks(filepath.Join(counterDir, window))
		if decodeErr != nil {
			return nil, decodeErr
		}
		out[i] = blocks
	}

	return out, nil
}

// convertWindows is the subprocess fallback: `go tool covdata textfmt` for
// each window, pairing each counters-only window with the package's single
// meta directory — covdata matches them by hash across input directories.
//
// The pool is bounded by the caller's share of the cores, not by GOMAXPROCS:
// this runs inside one of `jobs` package workers, and an inner bound that
// ignores the outer one multiplies into cores²/4 concurrent covdata processes —
// the exact trap the build path in this package already documents.
func convertWindows(
	ctx context.Context,
	opts Options,
	counterDir string,
	windows []string,
	buildParallelism int,
) ([][]cover.FileBlocks, error) {
	workers := min(max(1, buildParallelism), len(windows))

	out := make([][]cover.FileBlocks, len(windows))

	converting := pool.New().
		WithMaxGoroutines(workers).
		WithContext(ctx).
		WithFirstError().
		WithCancelOnError()

	for i, window := range windows {
		converting.Go(func(ctx context.Context) error {
			if err := ctx.Err(); err != nil {
				return err
			}

			input := filepath.Join(counterDir, window)
			meta := filepath.Join(counterDir, metaDirName)
			output := input + ".txt"
			if _, err := runCommand(ctx, counterDir, opts.Env, 0,
				"go", "tool", "covdata", "textfmt", "-i="+meta+","+input, "-o="+output); err != nil {
				return fmt.Errorf("convert coverage window %s: %w", window, err)
			}

			blocks, err := parseWindowFile(output)
			if err != nil {
				return err
			}
			out[i] = blocks

			return nil
		})
	}

	if err := converting.Wait(); err != nil {
		return nil, err
	}

	return out, nil
}

func parseWindowFile(path string) ([]cover.FileBlocks, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open converted window %s: %w", filepath.Base(path), err)
	}
	defer file.Close()

	blocks, err := cover.ParseProfile(file)
	if err != nil {
		return nil, fmt.Errorf("parse coverage window %s: %w", filepath.Base(path), err)
	}

	return blocks, nil
}

// rebuilderFrom continues a record's file interning where assembly left it, so
// salvaged tests share file ids with the harness-collected ones.
func rebuilderFrom(record *Record) *builder {
	build := newBuilder()
	for _, file := range record.Files {
		build.intern(file)
	}

	return build
}

func blocksToRanges(blocks []cover.FileBlocks, build *builder, resolve cover.Resolver) ([]Range, string) {
	var ranges []Range
	var unresolved string
	for _, fileBlocks := range blocks {
		abs, ok := resolve(fileBlocks.ProfileName)
		if !ok {
			if unresolved == "" {
				unresolved = fileBlocks.ProfileName
			}

			continue
		}
		id := build.intern(abs)
		for _, block := range fileBlocks.Blocks {
			ranges = append(ranges, Range{
				FileID: id,
				Start:  uint32(block.StartLine),
				End:    uint32(block.EndLine),
			})
		}
	}

	return ranges, unresolved
}
