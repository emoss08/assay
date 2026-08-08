package index

// In-process decoding of the Go toolchain's binary coverage files — the
// covmeta blob written once per package and the covcounters blob written per
// window. Shelling out to `go tool covdata textfmt` for every window meant one
// subprocess per test, each re-reading the package's whole-closure meta and
// serialising a text profile dominated by zero-count lines that the parser
// then discarded. On a large workspace that round-trip is the indexing wall
// clock. Decoding here reads the meta once per package and touches only the
// units a window actually executed.
//
// The formats are internal to the Go toolchain (src/internal/coverage/defs.go)
// and version-stamped: meta files carry MetaFileVersion 1, counter files
// CounterFileVersion 1, unchanged since Go 1.20. Every read is bounds-checked
// and every structural surprise — future version, foreign magic, hash
// mismatch, truncation — returns an error rather than a guess, and the caller
// falls back to the covdata subprocess path, which stays first-class. Being
// wrong here can cost a fallback; it must never cost a wrong record.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/emoss08/assay/internal/cover"
)

var (
	covMetaMagic    = [4]byte{0x00, 'c', 'v', 'm'}
	covCounterMagic = [4]byte{0x00, 'c', 'w', 'm'}
)

const (
	covMetaVersion    = 1
	covCounterVersion = 1

	// Counter flavors: how function records encode their integers.
	ctrRaw     = 1
	ctrULeb128 = 2

	// Counter granularities. Per-block is what -covermode emits; a per-func
	// meta attributes one counter to every unit of the function.
	granularityPerBlock = 1
	granularityPerFunc  = 2

	counterFilePrefix = "covcounters."
	metaFilePrefix    = "covmeta."
)

// errCorrupt is the base of every structural complaint; callers only ever
// treat decoding as all-or-nothing, so one sentinel is enough.
var errCorrupt = errors.New("unexpected coverage data layout")

// covMeta is one package's decoded meta-data file: the function tables for
// every instrumented package linked into the test binary, indexed by the
// package ID that counter records refer to.
type covMeta struct {
	hash     [16]byte
	perFunc  bool
	packages []covMetaPackage
}

type covMetaPackage struct {
	funcs []covMetaFunc
}

// covMetaFunc keeps only what window assembly consumes: the profile-style
// source name ("importpath/file.go", exactly what covdata textfmt prints) and
// each unit's line range, positionally matched to the function's counters.
type covMetaFunc struct {
	srcFile string
	units   []covUnit
}

type covUnit struct {
	start, end uint32
}

// byteReader reads little-endian scalars from an in-memory file with sticky
// error state, so decode paths read straight through and check once. The
// stdlib's slicereader panics on truncated input; these files come from disk
// and truncation must surface as an error.
type byteReader struct {
	b   []byte
	off int
	err error
}

func (r *byteReader) fail() {
	if r.err == nil {
		r.err = errCorrupt
	}
}

func (r *byteReader) bytes(n int) []byte {
	if r.err != nil || n < 0 || r.off+n > len(r.b) {
		r.fail()

		return nil
	}
	out := r.b[r.off : r.off+n]
	r.off += n

	return out
}

func (r *byteReader) u8() uint8 {
	b := r.bytes(1)
	if b == nil {
		return 0
	}

	return b[0]
}

func (r *byteReader) u32() uint32 {
	b := r.bytes(4)
	if b == nil {
		return 0
	}

	return binary.LittleEndian.Uint32(b)
}

func (r *byteReader) u32be() uint32 {
	b := r.bytes(4)
	if b == nil {
		return 0
	}

	return binary.BigEndian.Uint32(b)
}

func (r *byteReader) u64() uint64 {
	b := r.bytes(8)
	if b == nil {
		return 0
	}

	return binary.LittleEndian.Uint64(b)
}

func (r *byteReader) uleb() uint64 {
	var value uint64
	var shift uint
	for {
		b := r.bytes(1)
		if b == nil {
			return 0
		}
		value |= uint64(b[0]&0x7f) << shift
		if b[0]&0x80 == 0 {
			return value
		}
		shift += 7
		if shift > 63 {
			r.fail()

			return 0
		}
	}
}

func (r *byteReader) seek(off int) {
	if r.err != nil || off < 0 || off > len(r.b) {
		r.fail()

		return
	}
	r.off = off
}

// align4 skips padding up to the next 4-byte file offset, mirroring the
// preamble padding the counter writer inserts.
func (r *byteReader) align4() {
	if rem := r.off % 4; rem != 0 {
		r.bytes(4 - rem)
	}
}

// readStringTable decodes the uleb-prefixed string table both file kinds
// embed: entry count, then length-prefixed bytes per entry.
func (r *byteReader) readStringTable() []string {
	count := r.uleb()
	if r.err != nil || count > uint64(len(r.b)) {
		r.fail()

		return nil
	}
	out := make([]string, 0, count)
	for range count {
		out = append(out, string(r.bytes(int(r.uleb()))))
	}

	return out
}

// readMetaDir locates and decodes the single covmeta file the harness wrote
// into the package's meta directory.
func readMetaDir(dir string) (*covMeta, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read meta directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), metaFilePrefix) {
			continue
		}
		payload, readErr := os.ReadFile(filepath.Join(dir, entry.Name()))
		if readErr != nil {
			return nil, fmt.Errorf("read meta file: %w", readErr)
		}

		return decodeMeta(payload)
	}

	return nil, fmt.Errorf("%w: no %s* file in %s", errCorrupt, metaFilePrefix, dir)
}

func decodeMeta(payload []byte) (*covMeta, error) {
	r := &byteReader{b: payload}

	var magic [4]byte
	copy(magic[:], r.bytes(4))
	if magic != covMetaMagic {
		return nil, fmt.Errorf("%w: not a coverage meta file", errCorrupt)
	}
	if version := r.u32(); version != covMetaVersion {
		return nil, fmt.Errorf("%w: meta file version %d, can decode %d",
			errCorrupt, version, covMetaVersion)
	}

	totalLength := r.u64()
	entries := r.u64()
	if totalLength > uint64(len(payload)) || entries > uint64(len(payload)) {
		return nil, errCorrupt
	}

	meta := &covMeta{}
	copy(meta.hash[:], r.bytes(16))

	r.u32() // string table offset; the file-level table is not consulted
	r.u32() // string table length
	r.u8()  // counter mode
	meta.perFunc = r.u8() == granularityPerFunc
	r.bytes(6) // header padding

	offsets := make([]uint64, entries)
	for i := range offsets {
		offsets[i] = r.u64()
	}
	lengths := make([]uint64, entries)
	for i := range lengths {
		lengths[i] = r.u64()
	}
	if r.err != nil {
		return nil, r.err
	}

	meta.packages = make([]covMetaPackage, entries)
	for i := range meta.packages {
		end := offsets[i] + lengths[i]
		if offsets[i] > uint64(len(payload)) || end < offsets[i] || end > uint64(len(payload)) {
			return nil, errCorrupt
		}
		pkg, err := decodeMetaPackage(payload[offsets[i]:end])
		if err != nil {
			return nil, err
		}
		meta.packages[i] = pkg
	}

	return meta, nil
}

// decodeMetaPackage decodes one package's self-contained meta blob: symbol
// header, function offset table, package-local string table, then per-function
// unit lists.
func decodeMetaPackage(blob []byte) (covMetaPackage, error) {
	const symbolHeaderSize = 44 // coverage.CovMetaHeaderSize

	r := &byteReader{b: blob}
	if length := r.u32(); uint64(length) != uint64(len(blob)) {
		return covMetaPackage{}, errCorrupt
	}
	r.u32() // package name index
	r.u32() // package path index
	r.u32() // module path index
	r.bytes(16)
	r.bytes(4) // one unused byte plus padding
	r.u32()    // file count; files resolve through the string table
	numFuncs := r.u32()
	if r.err != nil || r.off != symbolHeaderSize || uint64(numFuncs) > uint64(len(blob)) {
		return covMetaPackage{}, errCorrupt
	}

	funcOffsets := make([]uint32, numFuncs)
	for i := range funcOffsets {
		funcOffsets[i] = r.u32()
	}

	strings := r.readStringTable()
	if r.err != nil {
		return covMetaPackage{}, r.err
	}

	pkg := covMetaPackage{funcs: make([]covMetaFunc, numFuncs)}
	for i, off := range funcOffsets {
		r.seek(int(off))
		numUnits := r.uleb()
		nameIdx := r.uleb()
		fileIdx := r.uleb()
		if r.err != nil || numUnits > uint64(len(blob)) ||
			nameIdx >= uint64(len(strings)) || fileIdx >= uint64(len(strings)) {
			return covMetaPackage{}, errCorrupt
		}

		fn := covMetaFunc{srcFile: strings[fileIdx], units: make([]covUnit, numUnits)}
		for u := range fn.units {
			stLine := r.uleb()
			r.uleb() // start column
			enLine := r.uleb()
			r.uleb() // end column
			r.uleb() // statement count
			fn.units[u] = covUnit{start: uint32(stLine), end: uint32(enLine)}
		}
		if r.err != nil {
			return covMetaPackage{}, r.err
		}
		pkg.funcs[i] = fn
	}

	return pkg, nil
}

// windowBlocks decodes every counter file in one window directory and returns
// the executed source blocks in the same shape ParseProfile produces from a
// text profile: per-file, count-filtered, merged, and sorted by profile name.
func (m *covMeta) windowBlocks(dir string) ([]cover.FileBlocks, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read counter directory: %w", err)
	}

	perFile := make(map[string][]cover.Block)
	found := false
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), counterFilePrefix) {
			continue
		}
		found = true
		payload, readErr := os.ReadFile(filepath.Join(dir, entry.Name()))
		if readErr != nil {
			return nil, fmt.Errorf("read counter file: %w", readErr)
		}
		if decodeErr := m.accumulateCounters(payload, perFile); decodeErr != nil {
			return nil, decodeErr
		}
	}
	if !found {
		return nil, fmt.Errorf("%w: no %s* file in %s", errCorrupt, counterFilePrefix, dir)
	}

	out := make([]cover.FileBlocks, 0, len(perFile))
	for file, blocks := range perFile {
		out = append(out, cover.FileBlocks{ProfileName: file, Blocks: cover.MergeBlocks(blocks)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProfileName < out[j].ProfileName })

	return out, nil
}

// accumulateCounters walks one counter file's segments and records the line
// ranges of every unit with a nonzero counter.
func (m *covMeta) accumulateCounters(payload []byte, perFile map[string][]cover.Block) error {
	const headerSize = 32
	const footerSize = 16

	r := &byteReader{b: payload}

	var magic [4]byte
	copy(magic[:], r.bytes(4))
	if magic != covCounterMagic {
		return fmt.Errorf("%w: not a coverage counter file", errCorrupt)
	}
	if version := r.u32(); version != covCounterVersion {
		return fmt.Errorf("%w: counter file version %d, can decode %d",
			errCorrupt, version, covCounterVersion)
	}

	var hash [16]byte
	copy(hash[:], r.bytes(16))
	if hash != m.hash {
		return fmt.Errorf("%w: counter file does not match the meta file", errCorrupt)
	}

	flavor := r.u8()
	bigEndian := r.u8() != 0
	r.bytes(6) // header padding

	var next func() uint32
	switch flavor {
	case ctrULeb128:
		next = func() uint32 { return uint32(r.uleb()) }
	case ctrRaw:
		if bigEndian {
			next = r.u32be
		} else {
			next = r.u32
		}
	default:
		return fmt.Errorf("%w: unknown counter flavor %d", errCorrupt, flavor)
	}

	if len(payload) < headerSize+footerSize {
		return errCorrupt
	}
	footer := &byteReader{b: payload, off: len(payload) - footerSize}
	copy(magic[:], footer.bytes(4))
	footer.bytes(4)
	segments := footer.u32()
	if magic != covCounterMagic || footer.err != nil {
		return errCorrupt
	}

	for segment := range segments {
		if segment > 0 {
			r.bytes(footerSize) // the previous segment's footer
		}

		fcnEntries := r.u64()
		strTabLen := r.u32()
		argsLen := r.u32()
		if r.err != nil || fcnEntries > uint64(len(payload)) {
			return errCorrupt
		}
		r.bytes(int(strTabLen))
		r.bytes(int(argsLen))
		r.align4()

		for range fcnEntries {
			numCounters := next()
			pkgIdx := next()
			funcIdx := next()
			if r.err != nil {
				return r.err
			}

			// An index outside the meta is ignored rather than fatal — the
			// same policy covdata itself applies to inconsistent payloads.
			var fn *covMetaFunc
			if int(pkgIdx) < len(m.packages) &&
				int(funcIdx) < len(m.packages[pkgIdx].funcs) {
				fn = &m.packages[pkgIdx].funcs[funcIdx]
			}

			for i := range int(numCounters) {
				count := next()
				if r.err != nil {
					return r.err
				}
				if count == 0 || fn == nil {
					continue
				}
				if m.perFunc {
					// One counter stands for the whole function.
					for _, unit := range fn.units {
						perFile[fn.srcFile] = append(perFile[fn.srcFile],
							cover.Block{StartLine: int(unit.start), EndLine: int(unit.end)})
					}

					continue
				}
				if i < len(fn.units) {
					unit := fn.units[i]
					perFile[fn.srcFile] = append(perFile[fn.srcFile],
						cover.Block{StartLine: int(unit.start), EndLine: int(unit.end)})
				}
			}
		}
	}

	return r.err
}
