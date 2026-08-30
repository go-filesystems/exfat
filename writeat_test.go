package filesystem_exfat

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

// probeWritable asserts the capability is reachable the way a caller reaches
// it — through the filesystem.File that OpenFile returns, not the concrete
// type — and hands back the WritableFile.
func probeWritable(t *testing.T, f filesystem.File) filesystem.WritableFile {
	t.Helper()
	w, ok := f.(filesystem.WritableFile)
	if !ok {
		t.Fatal("exfat's File does not satisfy filesystem.WritableFile")
	}
	return w
}

// wpattern builds deterministic, position-dependent bytes. A constant fill
// would hide an off-by-one-cluster: every wrong byte would happen to be right.
func wpattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31) ^ seed ^ byte(i>>8)
	}
	return b
}

// readModifyWrite is the slow path this whole capability exists to replace:
// read the entire file, splice, write the entire file back. It is the ORACLE
// for every WriteAt below — the two must agree byte for byte, because a caller
// that falls back to it when a driver has no WritableFile must get the same
// filesystem either way.
func readModifyWrite(t *testing.T, fsIfc filesystem.Filesystem, path string, p []byte, off int64) {
	t.Helper()
	cur, err := fsIfc.ReadFile(path)
	if err != nil {
		t.Fatalf("oracle ReadFile(%s): %v", path, err)
	}
	if end := off + int64(len(p)); end > int64(len(cur)) {
		grown := make([]byte, end)
		copy(grown, cur)
		cur = grown
	}
	copy(cur[off:], p)
	if err := fsIfc.WriteFile(path, cur, 0o644); err != nil {
		t.Fatalf("oracle WriteFile(%s): %v", path, err)
	}
}

// checkReadPathsAgree reads the file back BOTH ways — ReadAt on a freshly
// opened File, and ReadFile — and requires them to be identical. The two use
// different code (a materialising cluster walk, versus offset arithmetic over
// a resolved cluster list), so a write that updated one view and not the other
// is caught here and nowhere else.
func checkReadPathsAgree(t *testing.T, fsIfc filesystem.Filesystem, path string, want []byte) {
	t.Helper()
	viaReadFile, err := fsIfc.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	if !bytes.Equal(viaReadFile, want) {
		t.Fatalf("%s: ReadFile gave %d bytes, want %d; first difference at %d",
			path, len(viaReadFile), len(want), firstDiff(viaReadFile, want))
	}
	f, err := fsIfc.(filesystem.Opener).OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile(%s): %v", path, err)
	}
	defer f.Close()
	if got := f.Size(); got != int64(len(want)) {
		t.Fatalf("%s: Size() = %d, want %d", path, got, len(want))
	}
	viaReadAt := make([]byte, len(want))
	if len(want) > 0 {
		n, err := f.ReadAt(viaReadAt, 0)
		if n != len(want) || (err != nil && !errors.Is(err, io.EOF)) {
			t.Fatalf("%s: ReadAt(all) = %d, %v", path, n, err)
		}
	}
	if !bytes.Equal(viaReadAt, want) {
		t.Fatalf("%s: ReadAt disagrees with the expected content at byte %d",
			path, firstDiff(viaReadAt, want))
	}
}

func firstDiff(a, b []byte) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

// ── image builders ────────────────────────────────────────────────────────

// formatImage returns a fresh volume this package formatted itself.
func formatImage(t *testing.T, name string) filesystem.Filesystem {
	t.Helper()
	fsIfc, err := Format(filepath.Join(t.TempDir(), name), 16*1024*1024, FormatConfig{Label: "WRITEAT"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	t.Cleanup(func() { _ = fsIfc.Close() })
	return fsIfc
}

// mkfsImage returns the committed fixture produced by macOS newfs_exfat — a
// geometry, and a set of on-disk conventions, this package did not choose.
func mkfsImage(t *testing.T) filesystem.Filesystem {
	t.Helper()
	src := filepath.Join("testdata", "mkfs", "image.exfat.gz")
	if _, err := os.Stat(src); err != nil {
		t.Skipf("fixture %s missing: %v", src, err)
	}
	dst := filepath.Join(t.TempDir(), "image.exfat")
	if err := extractGz(src, dst); err != nil {
		t.Fatalf("extractGz: %v", err)
	}
	fsIfc, err := Open(dst, -1)
	if err != nil {
		t.Fatalf("Open fixture: %v", err)
	}
	t.Cleanup(func() { _ = fsIfc.Close() })
	return fsIfc
}

// makeNoFatChain converts an existing file into a genuine NoFatChain
// (contiguous) file: it erases every FAT entry the file owns and sets
// GeneralSecondaryFlags bit 1 in its Stream Extension entry, leaving the
// Allocation Bitmap bits set, which is exactly the shape a canonical formatter
// produces. It asserts the file really was laid out consecutively first, and
// that the FAT really is empty afterwards, so a test using it cannot pass by
// accident on a still-chained file.
func makeNoFatChain(t *testing.T, fs *exfatFS, path string) {
	t.Helper()
	entry, _, err := fs.resolvePath(path)
	if err != nil {
		t.Fatalf("resolvePath(%s): %v", path, err)
	}
	clusters, _, err := fs.chainClusters(entry.cluster, entry.size)
	if err != nil {
		t.Fatalf("chainClusters(%s): %v", path, err)
	}
	if len(clusters) < 2 {
		t.Fatalf("%s occupies %d clusters — too few to prove anything about a contiguous run", path, len(clusters))
	}
	for i := 1; i < len(clusters); i++ {
		if clusters[i] != clusters[i-1]+1 {
			t.Fatalf("%s is not laid out consecutively (%v) — cannot be made NoFatChain", path, clusters)
		}
	}
	for _, c := range clusters {
		if err := fs.setFATEntry(c, 0); err != nil {
			t.Fatalf("setFATEntry: %v", err)
		}
	}

	name, parentCluster, err := fs.getParentDir(path)
	if err != nil {
		t.Fatalf("getParentDir: %v", err)
	}
	buf, err := fs.readDirBuf(parentCluster)
	if err != nil {
		t.Fatalf("readDirBuf: %v", err)
	}
	off, secondaryCount := exfatFindEntry(buf, name)
	if off < 0 {
		t.Fatalf("%s not found in its directory", path)
	}
	buf[off+dirEntrySize+1] |= exfatFlagNoFatChain
	setLen := (secondaryCount + 1) * dirEntrySize
	binary.LittleEndian.PutUint16(buf[off+2:off+4], exfatEntrySetChecksum(buf[off:off+setLen]))
	if err := fs.writeDirBuf(parentCluster, buf); err != nil {
		t.Fatalf("writeDirBuf: %v", err)
	}

	// Prove the FAT really holds nothing for the file now: without this, a
	// "contiguous" test could be passing on a file that is still chained.
	fatBase := fs.info.FATOffsetBytes(fs.partOffset)
	for _, c := range clusters {
		var b [4]byte
		if _, err := fs.f.ReadAt(b[:], fatBase+int64(c)*4); err != nil {
			t.Fatalf("read FAT: %v", err)
		}
		if v := binary.LittleEndian.Uint32(b[:]); v != 0 {
			t.Fatalf("FAT entry for cluster %d = %#x after conversion, want 0", c, v)
		}
	}
	t.Logf("%s: %d consecutive clusters %d..%d, NoFatChain set, FAT emptied",
		path, len(clusters), clusters[0], clusters[len(clusters)-1])
}

// ── the sweep ─────────────────────────────────────────────────────────────

// writeCase is one positional write to prove equivalent to read-modify-write.
type writeCase struct {
	name string
	off  func(clusterSize, size int64) int64
	n    func(clusterSize, size int64) int
}

// writeCases covers, on every geometry: writes wholly inside one cluster, a
// write straddling a cluster boundary, a write exactly on one, a write that
// extends the file, and a write landing in a HOLE past the end.
var writeCases = []writeCase{
	{"start", func(cs, sz int64) int64 { return 0 }, func(cs, sz int64) int { return 17 }},
	{"interior-within-cluster", func(cs, sz int64) int64 { return cs + 5 }, func(cs, sz int64) int { return int(cs) - 10 }},
	{"straddles-one-boundary", func(cs, sz int64) int64 { return cs - 3 }, func(cs, sz int64) int { return 9 }},
	{"straddles-many-boundaries", func(cs, sz int64) int64 { return cs/2 + 1 }, func(cs, sz int64) int { return int(3*cs) + 7 }},
	{"exactly-on-boundary", func(cs, sz int64) int64 { return 2 * cs }, func(cs, sz int64) int { return int(cs) }},
	{"last-byte", func(cs, sz int64) int64 { return sz - 1 }, func(cs, sz int64) int { return 1 }},
	{"extends-within-last-cluster", func(cs, sz int64) int64 { return sz }, func(cs, sz int64) int { return 3 }},
	{"extends-past-last-cluster", func(cs, sz int64) int64 { return sz - 2 }, func(cs, sz int64) int { return int(2*cs) + 11 }},
	{"hole-inside-last-cluster", func(cs, sz int64) int64 { return sz + 4 }, func(cs, sz int64) int { return 6 }},
	{"hole-spanning-whole-clusters", func(cs, sz int64) int64 { return sz + 3*cs + 9 }, func(cs, sz int64) int { return int(cs) + 1 }},
	{"whole-file-overwrite", func(cs, sz int64) int64 { return 0 }, func(cs, sz int64) int { return int(sz) }},
}

// runWriteCases is THE verification the capability has to survive: on a real
// image, for every offset shape above, WriteAt must produce exactly the same
// file as ReadFile + splice + WriteFile, and the result must read back the
// same through ReadAt and through ReadFile.
//
// Each case gets its own pair of freshly built images, so a case cannot be
// masked by the state a previous one left behind. prepare, when non-nil, is
// applied to BOTH images after seeding — it is how the contiguous
// (NoFatChain) variant of the whole sweep is obtained.
func runWriteCases(t *testing.T, mk func(t *testing.T, name string) filesystem.Filesystem, prepare func(*testing.T, *exfatFS, string)) {
	t.Helper()
	for _, tc := range writeCases {
		t.Run(tc.name, func(t *testing.T) {
			mine := mk(t, "mine.img")
			oracle := mk(t, "oracle.img")
			clusterSize := int64(mine.(*exfatFS).info.ClusterSize())

			const path = "/DATA.BIN"
			initial := wpattern(int(clusterSize)*6+37, 0x5A)
			for _, fsIfc := range []filesystem.Filesystem{mine, oracle} {
				if err := fsIfc.WriteFile(path, initial, 0o644); err != nil {
					t.Fatalf("seed WriteFile: %v", err)
				}
				if prepare != nil {
					prepare(t, fsIfc.(*exfatFS), path)
				}
			}
			size := int64(len(initial))
			off := tc.off(clusterSize, size)
			data := wpattern(tc.n(clusterSize, size), 0xC7)

			// The path under test.
			f, err := mine.(filesystem.Opener).OpenFile(path)
			if err != nil {
				t.Fatalf("OpenFile: %v", err)
			}
			w := probeWritable(t, f)
			n, err := w.WriteAt(data, off)
			if n != len(data) || err != nil {
				t.Fatalf("WriteAt(len=%d, off=%d) = %d, %v — io.WriterAt requires all of p or an error",
					len(data), off, n, err)
			}
			wantSize := max(size, off+int64(len(data)))
			if got := w.Size(); got != wantSize {
				t.Fatalf("Size() = %d after WriteAt, want %d — a WritableFile's Size must follow its own writes",
					got, wantSize)
			}
			if err := w.Sync(); err != nil {
				t.Fatalf("Sync: %v", err)
			}
			if err := w.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			// The oracle.
			readModifyWrite(t, oracle, path, data, off)
			want, err := oracle.ReadFile(path)
			if err != nil {
				t.Fatalf("oracle ReadFile: %v", err)
			}
			if int64(len(want)) != wantSize {
				t.Fatalf("oracle produced %d bytes, want %d", len(want), wantSize)
			}

			// They must agree, and both read paths must agree with them.
			checkReadPathsAgree(t, mine, path, want)

			// A Stat through the Filesystem must see the new length too:
			// exFAT keeps it in the Stream Extension entry, so a WriteAt that
			// forgot to rewrite the entry set would pass every check above
			// and fail here.
			st, err := mine.Stat(path)
			if err != nil {
				t.Fatalf("Stat: %v", err)
			}
			if int64(st.Size()) != wantSize {
				t.Fatalf("Stat().Size() = %d, want %d", st.Size(), wantSize)
			}
		})
	}
}

// TestWriteAtOnMkfsImage runs the whole sweep on the volume macOS newfs_exfat
// produced — a foreign layout, and one whose files are all NoFatChain, so the
// allocator has to respect an allocation the FAT says nothing about.
func TestWriteAtOnMkfsImage(t *testing.T) {
	runWriteCases(t, func(t *testing.T, _ string) filesystem.Filesystem { return mkfsImage(t) }, nil)
}

// TestWriteAtOnFormattedImage repeats the sweep on a volume this package
// formats itself, where the seed file owns a real FAT chain.
func TestWriteAtOnFormattedImage(t *testing.T) {
	runWriteCases(t, formatImage, nil)
}

// TestWriteAtOnContiguousFile repeats the entire sweep with the seed file
// converted to a NoFatChain, consecutive allocation. This is the exFAT-specific
// half of the problem and the one place a bug would hide: the file's clusters
// are addressed without the FAT, its FAT entries read as zero (i.e. as free
// space), and any change of length has to materialise a real chain first.
func TestWriteAtOnContiguousFile(t *testing.T) {
	runWriteCases(t, formatImage, makeNoFatChain)
}

// TestNoFatChainReadPathsAgreeBeforeAnyWrite is the prerequisite the write
// capability forced into the open: a multi-cluster NoFatChain file must read
// correctly through ReadFile AND through OpenFile/ReadAt. Walking the FAT for
// such a file stops after its first cluster, which is why the oracle above
// would otherwise be comparing against a truncated file.
func TestNoFatChainReadPathsAgreeBeforeAnyWrite(t *testing.T) {
	fsIfc := formatImage(t, "nofat.img")
	fs := fsIfc.(*exfatFS)
	cs := int(fs.info.ClusterSize())
	want := wpattern(cs*5+123, 0x4D)
	if err := fsIfc.WriteFile("/CONTIG.BIN", want, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	makeNoFatChain(t, fs, "/CONTIG.BIN")
	checkReadPathsAgree(t, fsIfc, "/CONTIG.BIN", want)

	// The file's own Stat must still report the real length.
	st, err := fsIfc.Stat("/CONTIG.BIN")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.Size() != uint64(len(want)) {
		t.Fatalf("Stat().Size() = %d, want %d", st.Size(), len(want))
	}
}

// TestWriteAtInPlaceKeepsContiguity documents the rule: a write that does not
// change the length touches no metadata at all, so a NoFatChain file stays
// NoFatChain — cheaper, and still exactly what the entry says it is.
func TestWriteAtInPlaceKeepsContiguity(t *testing.T) {
	fsIfc := formatImage(t, "inplace.img")
	fs := fsIfc.(*exfatFS)
	cs := int(fs.info.ClusterSize())
	want := wpattern(cs*4, 0x6E)
	if err := fsIfc.WriteFile("/C.BIN", want, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	makeNoFatChain(t, fs, "/C.BIN")

	f, err := fsIfc.(filesystem.Opener).OpenFile("/C.BIN")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if !f.(*exfatFile).contiguous {
		t.Fatal("OpenFile did not notice the NoFatChain flag")
	}
	w := probeWritable(t, f)
	p := wpattern(2*cs+9, 0x7F)
	if _, err := w.WriteAt(p, int64(cs)-4); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	copy(want[cs-4:], p)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	entry, _, err := fs.resolvePath("/C.BIN")
	if err != nil {
		t.Fatalf("resolvePath: %v", err)
	}
	if entry.flags&exfatFlagNoFatChain == 0 {
		t.Fatal("an in-place write cleared NoFatChain — it should not touch the entry at all")
	}
	checkReadPathsAgree(t, fsIfc, "/C.BIN", want)
}

// TestGrowMaterialisesTheChain is the other half of the rule: the moment the
// length changes, the file becomes a canonical chained file, because a grow
// cannot assume the cluster after the run is free. The test proves the flag is
// cleared AND that the FAT now really describes the file, by reading the chain
// back through the ordinary walk.
func TestGrowMaterialisesTheChain(t *testing.T) {
	fsIfc := formatImage(t, "grow.img")
	fs := fsIfc.(*exfatFS)
	cs := int(fs.info.ClusterSize())
	head := wpattern(cs*3, 0x2B)
	if err := fsIfc.WriteFile("/G.BIN", head, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// Park a second file immediately after it so the run CANNOT simply be
	// extended in place — the next cluster is taken.
	if err := fsIfc.WriteFile("/BLOCKER.BIN", wpattern(cs*2, 0x3C), 0o644); err != nil {
		t.Fatalf("WriteFile blocker: %v", err)
	}
	makeNoFatChain(t, fs, "/G.BIN")

	f, err := fsIfc.(filesystem.Opener).OpenFile("/G.BIN")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	w := probeWritable(t, f)
	tail := wpattern(cs+17, 0x5D)
	if _, err := w.WriteAt(tail, int64(cs*3)); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	entry, _, err := fs.resolvePath("/G.BIN")
	if err != nil {
		t.Fatalf("resolvePath: %v", err)
	}
	if entry.flags&exfatFlagNoFatChain != 0 {
		t.Fatal("a grow left NoFatChain set — the file now claims a contiguity it has not got")
	}
	clusters, covered, err := fs.chainClusters(entry.cluster, entry.size)
	if err != nil {
		t.Fatalf("chainClusters: %v", err)
	}
	if covered != int64(len(head)+len(tail)) {
		t.Fatalf("the materialised chain covers %d bytes, want %d", covered, len(head)+len(tail))
	}
	// Derived, not written down: the file is now len(head)+len(tail) bytes,
	// i.e. four whole clusters and 17 bytes, so it needs five. A hard-coded
	// count here is a second place to get the arithmetic wrong, and the count
	// is the whole point of the assertion.
	wantClusters := (len(head) + len(tail) + cs - 1) / cs
	if len(clusters) != wantClusters {
		t.Fatalf("materialised chain has %d clusters, want %d for %d bytes at %d bytes per cluster",
			len(clusters), wantClusters, len(head)+len(tail), cs)
	}
	want := append(append([]byte{}, head...), tail...)
	checkReadPathsAgree(t, fsIfc, "/G.BIN", want)
	// The blocker must be untouched: growing over it would be the corruption
	// this whole design exists to avoid.
	checkReadPathsAgree(t, fsIfc, "/BLOCKER.BIN", wpattern(cs*2, 0x3C))
}

// TestAllocatorRefusesNoFatChainClusters is the regression test for the reason
// the allocator consults the Allocation Bitmap. On exFAT a NoFatChain file's
// FAT entries are ZERO — a FAT-only free scan sees them as free space and
// hands them straight out. Every file in the newfs_exfat fixture is NoFatChain,
// so this is what "writing into a canonical image" used to mean.
func TestAllocatorRefusesNoFatChainClusters(t *testing.T) {
	fsIfc := mkfsImage(t)
	fs := fsIfc.(*exfatFS)

	occupied := map[uint32]string{}
	for _, p := range []string{"/hello.txt", "/sub/blob.bin"} {
		entry, _, err := fs.resolvePath(p)
		if err != nil {
			t.Fatalf("resolvePath(%s): %v", p, err)
		}
		if entry.flags&exfatFlagNoFatChain == 0 {
			t.Fatalf("%s is not NoFatChain in the fixture — this test no longer proves anything", p)
		}
		clusters, _ := fs.contiguousClusters(entry.cluster, entry.size)
		for _, c := range clusters {
			occupied[c] = p
			var b [4]byte
			if _, err := fs.f.ReadAt(b[:], fs.info.FATOffsetBytes(fs.partOffset)+int64(c)*4); err != nil {
				t.Fatalf("read FAT: %v", err)
			}
			if v := binary.LittleEndian.Uint32(b[:]); v != 0 {
				t.Fatalf("cluster %d of %s has FAT entry %#x; the fixture is not what this test assumes", c, p, v)
			}
		}
	}
	if len(occupied) == 0 {
		t.Fatal("no NoFatChain clusters found in the fixture")
	}

	run, err := fs.allocClusterRun(20)
	if err != nil {
		t.Fatalf("allocClusterRun: %v", err)
	}
	for _, c := range run {
		if p, bad := occupied[c]; bad {
			t.Fatalf("the allocator offered cluster %d, which %s occupies", c, p)
		}
	}

	// And end to end: writing a new file must not damage the existing ones.
	if err := fsIfc.WriteFile("/NEW.BIN", wpattern(9000, 0x8A), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got, err := fsIfc.ReadFile("/hello.txt"); err != nil || string(got) != "hello\n" {
		t.Fatalf("/hello.txt after writing another file = %q, %v", got, err)
	}
	blob, err := fsIfc.ReadFile("/sub/blob.bin")
	if err != nil {
		t.Fatalf("ReadFile blob: %v", err)
	}
	if len(blob) != 1024 || !bytes.Equal(blob, bytes.Repeat([]byte("A"), 1024)) {
		t.Fatalf("/sub/blob.bin was damaged: %d bytes", len(blob))
	}
}

// TestWriteAtSequentialMatchesWholeFile is the shape the NFS server produces
// and the reason this capability exists: a file written from offset zero in
// fixed-size blocks. Before WriteAt each block cost a full read-modify-write,
// so the total was quadratic; here it must still land byte-for-byte identical
// to a single whole-file write of the same bytes.
func TestWriteAtSequentialMatchesWholeFile(t *testing.T) {
	const path = "/SEQ.BIN"
	const block = 32 * 1024
	whole := wpattern(block*13+555, 0x3C)

	incremental := formatImage(t, "incremental.img")
	if err := incremental.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("create: %v", err)
	}
	f, err := incremental.(filesystem.Opener).OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	w := probeWritable(t, f)
	for off := 0; off < len(whole); off += block {
		end := min(off+block, len(whole))
		if n, err := w.WriteAt(whole[off:end], int64(off)); n != end-off || err != nil {
			t.Fatalf("WriteAt(off=%d) = %d, %v", off, n, err)
		}
		// The client's next GETATTR must already see the file grow, which is
		// what makes an appending stream over NFS behave.
		if got := w.Size(); got != int64(end) {
			t.Fatalf("Size() = %d after writing through %d, want %d", got, end, end)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	checkReadPathsAgree(t, incremental, path, whole)

	atOnce := formatImage(t, "atonce.img")
	if err := atOnce.WriteFile(path, whole, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	want, err := atOnce.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	got, err := incremental.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("block-by-block WriteAt differs from a single WriteFile at byte %d", firstDiff(got, want))
	}
}

// TestWriteAtIntoFragmentedChain proves the offset→cluster mapping on a chain
// that is NOT contiguous. A contiguous layout would let a wrong mapping (say,
// "first cluster plus index") pass every other test in this file.
func TestWriteAtIntoFragmentedChain(t *testing.T) {
	fsIfc := formatImage(t, "frag.img")
	fs := fsIfc.(*exfatFS)
	cs := int(fs.info.ClusterSize())

	// Fill A, then B, free A, then write C larger than A's hole: C takes A's
	// freed clusters and continues past B, so its chain jumps around B's.
	if err := fsIfc.WriteFile("/A.BIN", wpattern(cs*20, 0xA1), 0o644); err != nil {
		t.Fatalf("WriteFile A: %v", err)
	}
	if err := fsIfc.WriteFile("/B.BIN", wpattern(cs*8, 0xB2), 0o644); err != nil {
		t.Fatalf("WriteFile B: %v", err)
	}
	if err := fsIfc.DeleteFile("/A.BIN"); err != nil {
		t.Fatalf("DeleteFile A: %v", err)
	}
	want := wpattern(cs*50+123, 0xC3)
	if err := fsIfc.WriteFile("/C.BIN", want, 0o644); err != nil {
		t.Fatalf("WriteFile C: %v", err)
	}

	entry, _, err := fs.resolvePath("/C.BIN")
	if err != nil {
		t.Fatalf("resolvePath: %v", err)
	}
	clusters, _, err := fs.chainClusters(entry.cluster, entry.size)
	if err != nil {
		t.Fatalf("chainClusters: %v", err)
	}
	disc := 0
	for i := 1; i < len(clusters); i++ {
		if clusters[i] != clusters[i-1]+1 {
			disc++
		}
	}
	if disc == 0 {
		t.Fatalf("/C.BIN chain is contiguous (%d clusters) — the fragmentation setup did not take", len(clusters))
	}
	t.Logf("/C.BIN: %d clusters, %d discontinuities", len(clusters), disc)

	f, err := fsIfc.(filesystem.Opener).OpenFile("/C.BIN")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	w := probeWritable(t, f)
	// Write across every discontinuity in the chain: at each jump, a span
	// straddling the boundary between the two non-adjacent clusters.
	for i := 1; i < len(clusters); i++ {
		if clusters[i] == clusters[i-1]+1 {
			continue
		}
		off := int64(i*cs) - 5
		p := wpattern(11, byte(i))
		if n, err := w.WriteAt(p, off); n != len(p) || err != nil {
			t.Fatalf("WriteAt across discontinuity at cluster index %d: %d, %v", i, n, err)
		}
		copy(want[off:], p)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	checkReadPathsAgree(t, fsIfc, "/C.BIN", want)
}

// TestWriteAtHoleReadsAsZeros pins the rule a caller cannot check any other
// way: bytes never written, between the old end of file and a write past it,
// must read back as zeros through BOTH read paths — not as whatever the
// clusters held when some earlier, deleted file owned them.
func TestWriteAtHoleReadsAsZeros(t *testing.T) {
	fsIfc := formatImage(t, "hole.img")
	cs := int(fsIfc.(*exfatFS).info.ClusterSize())

	// Dirty the data region first: write a file of 0xFF, delete it, so the
	// clusters a later grow allocates are certainly not already zero.
	dirty := bytes.Repeat([]byte{0xFF}, cs*12)
	if err := fsIfc.WriteFile("/DIRTY.BIN", dirty, 0o644); err != nil {
		t.Fatalf("WriteFile dirty: %v", err)
	}
	if err := fsIfc.DeleteFile("/DIRTY.BIN"); err != nil {
		t.Fatalf("DeleteFile dirty: %v", err)
	}

	head := wpattern(10, 0x11)
	if err := fsIfc.WriteFile("/SPARSE.BIN", head, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	f, err := fsIfc.(filesystem.Opener).OpenFile("/SPARSE.BIN")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	w := probeWritable(t, f)
	tail := wpattern(7, 0x22)
	holeAt := int64(cs*5 + 3)
	if _, err := w.WriteAt(tail, holeAt); err != nil {
		t.Fatalf("WriteAt past end: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	want := make([]byte, int(holeAt)+len(tail))
	copy(want, head)
	copy(want[holeAt:], tail)
	checkReadPathsAgree(t, fsIfc, "/SPARSE.BIN", want)
}

// TestTruncateFile exercises the file-scoped Truncate in both directions and
// checks it against the path-scoped one on an identical image: the two must
// leave the same file, since a caller may reach either.
func TestTruncateFile(t *testing.T) {
	const path = "/T.BIN"
	for _, size := range []int64{0, 1, 4095, 4096, 4097, 20000, 40000} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			mine, oracle := formatImage(t, "m.img"), formatImage(t, "o.img")
			initial := wpattern(9000, 0x77)
			for _, fsIfc := range []filesystem.Filesystem{mine, oracle} {
				if err := fsIfc.WriteFile(path, initial, 0o644); err != nil {
					t.Fatalf("seed: %v", err)
				}
			}
			f, err := mine.(filesystem.Opener).OpenFile(path)
			if err != nil {
				t.Fatalf("OpenFile: %v", err)
			}
			w := probeWritable(t, f)
			if err := w.Truncate(size); err != nil {
				t.Fatalf("Truncate(%d): %v", size, err)
			}
			if got := w.Size(); got != size {
				t.Fatalf("Size() = %d after Truncate(%d)", got, size)
			}
			// Truncating to the size it already has must be a no-op that
			// still succeeds — the early return.
			if err := w.Truncate(size); err != nil {
				t.Fatalf("Truncate(%d) second time: %v", size, err)
			}
			if err := w.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			if err := oracle.(filesystem.Truncater).Truncate(path, size); err != nil {
				t.Fatalf("path-scoped Truncate(%d): %v", size, err)
			}
			want, err := oracle.ReadFile(path)
			if err != nil {
				t.Fatalf("oracle ReadFile: %v", err)
			}
			checkReadPathsAgree(t, mine, path, want)
		})
	}
}

// TestTruncateContiguousFile runs the same comparison on a NoFatChain file, in
// both directions: the file-scoped Truncate materialises a chain, the
// path-scoped one re-emits the body, and the two must still agree.
func TestTruncateContiguousFile(t *testing.T) {
	const path = "/TC.BIN"
	for _, size := range []int64{0, 4096, 5000, 20000, 40000} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			mine, oracle := formatImage(t, "m.img"), formatImage(t, "o.img")
			initial := wpattern(9000, 0x21)
			for _, fsIfc := range []filesystem.Filesystem{mine, oracle} {
				if err := fsIfc.WriteFile(path, initial, 0o644); err != nil {
					t.Fatalf("seed: %v", err)
				}
				makeNoFatChain(t, fsIfc.(*exfatFS), path)
			}
			f, err := mine.(filesystem.Opener).OpenFile(path)
			if err != nil {
				t.Fatalf("OpenFile: %v", err)
			}
			w := probeWritable(t, f)
			if err := w.Truncate(size); err != nil {
				t.Fatalf("Truncate(%d): %v", size, err)
			}
			if err := w.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if err := oracle.(filesystem.Truncater).Truncate(path, size); err != nil {
				t.Fatalf("path-scoped Truncate(%d): %v", size, err)
			}
			want, err := oracle.ReadFile(path)
			if err != nil {
				t.Fatalf("oracle ReadFile: %v", err)
			}
			checkReadPathsAgree(t, mine, path, want)
		})
	}
}

// TestTruncateGrowZeroFills: growing by Truncate must zero-fill, over clusters
// that certainly held something else.
func TestTruncateGrowZeroFills(t *testing.T) {
	fsIfc := formatImage(t, "tg.img")
	if err := fsIfc.WriteFile("/D.BIN", bytes.Repeat([]byte{0xEE}, 40000), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := fsIfc.DeleteFile("/D.BIN"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	head := wpattern(100, 0x01)
	if err := fsIfc.WriteFile("/G.BIN", head, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	f, err := fsIfc.(filesystem.Opener).OpenFile("/G.BIN")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	w := probeWritable(t, f)
	if err := w.Truncate(30000); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	want := make([]byte, 30000)
	copy(want, head)
	checkReadPathsAgree(t, fsIfc, "/G.BIN", want)
}

// TestWriteAtConcurrentDisjointRanges: io.WriterAt permits parallel writes to
// non-overlapping ranges, and a mount issues exactly that. Under -race this
// also proves the File's own state is not torn by a concurrent extend.
func TestWriteAtConcurrentDisjointRanges(t *testing.T) {
	fsIfc := formatImage(t, "conc.img")
	const n, chunk = 32, 4096
	want := wpattern(n*chunk, 0x9E)
	if err := fsIfc.WriteFile("/C.BIN", make([]byte, n*chunk), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	f, err := fsIfc.(filesystem.Opener).OpenFile("/C.BIN")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	w := probeWritable(t, f)

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			off := int64(i * chunk)
			_, errs[i] = w.WriteAt(want[off:off+chunk], off)
		}()
	}
	// Reads run alongside: io.ReaderAt calls stay parallel-safe throughout.
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = w.ReadAt(make([]byte, 512), 0)
			_ = w.Size()
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent WriteAt %d: %v", i, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	checkReadPathsAgree(t, fsIfc, "/C.BIN", want)
}

// ── contract and error branches ───────────────────────────────────────────

// errInjected is the sentinel returned by the fault-injecting diskRW below.
var errInjected = errors.New("exfat: injected disk error")

// faultRW wraps a real diskRW and fails I/O selectively, by half-open byte
// region on a given operation. A region is deterministic regardless of how
// many unrelated calls precede the targeted FAT / bitmap / data access, which
// matters here because a single WriteAt touches three different regions.
type faultRW struct {
	inner                    diskRW
	failReadLo, failReadHi   int64
	failWriteLo, failWriteHi int64
	readRegionSkip           int
	writeRegionSkip          int
	readRegionHits           int
	writeRegionHits          int
}

func (f *faultRW) ReadAt(p []byte, off int64) (int, error) {
	if f.failReadHi > f.failReadLo && off >= f.failReadLo && off < f.failReadHi {
		f.readRegionHits++
		if f.readRegionHits > f.readRegionSkip {
			return 0, errInjected
		}
	}
	return f.inner.ReadAt(p, off)
}

func (f *faultRW) WriteAt(p []byte, off int64) (int, error) {
	if f.failWriteHi > f.failWriteLo && off >= f.failWriteLo && off < f.failWriteHi {
		f.writeRegionHits++
		if f.writeRegionHits > f.writeRegionSkip {
			return 0, errInjected
		}
	}
	return f.inner.WriteAt(p, off)
}

func (f *faultRW) Close() error { return f.inner.Close() }

func fatRegion(fs *exfatFS) (lo, hi int64) {
	lo = fs.info.FATOffsetBytes(fs.partOffset)
	return lo, lo + int64(fs.info.FATLength)*int64(fs.info.BytesPerSector())
}

// bitmapRegion is the Allocation Bitmap's own bytes. It lives INSIDE the
// cluster heap, so it has to be excluded from "data" faults and targeted
// separately.
func bitmapRegion(fs *exfatFS) (lo, hi int64) {
	lo = fs.info.ClusterHeapOffsetBytes(fs.partOffset) + int64(fs.bitmapCluster-2)*int64(fs.info.ClusterSize())
	return lo, lo + int64(fs.bitmapLength)
}

// userDataRegion starts at the first cluster past the three system clusters a
// Format image reserves (bitmap, up-case table, root directory), so a fault
// armed on it hits file data and nothing else.
func userDataRegion(fs *exfatFS) (lo, hi int64) {
	lo = fs.info.ClusterHeapOffsetBytes(fs.partOffset) + int64(fs.info.RootDirectoryCluster-1)*int64(fs.info.ClusterSize())
	return lo, 1 << 62
}

func (f *faultRW) failFATReads(fs *exfatFS)     { f.failReadLo, f.failReadHi = fatRegion(fs) }
func (f *faultRW) failFATWrites(fs *exfatFS)    { f.failWriteLo, f.failWriteHi = fatRegion(fs) }
func (f *faultRW) failBitmapReads(fs *exfatFS)  { f.failReadLo, f.failReadHi = bitmapRegion(fs) }
func (f *faultRW) failBitmapWrites(fs *exfatFS) { f.failWriteLo, f.failWriteHi = bitmapRegion(fs) }
func (f *faultRW) failDataWrites(fs *exfatFS)   { f.failWriteLo, f.failWriteHi = userDataRegion(fs) }

// wrapFault swaps the filesystem's backing diskRW for a faultRW and returns it
// so the caller can arm fault points after the file has been populated.
func wrapFault(fs *exfatFS) *faultRW {
	w := &faultRW{inner: fs.f}
	fs.f = w
	return w
}

// openWritable formats an image, creates a file, and returns it opened.
func openWritable(t *testing.T, initial []byte) (*exfatFS, filesystem.WritableFile) {
	t.Helper()
	fsIfc := formatImage(t, "w.img")
	if err := fsIfc.WriteFile("/W.BIN", initial, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	f, err := fsIfc.(filesystem.Opener).OpenFile("/W.BIN")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return fsIfc.(*exfatFS), probeWritable(t, f)
}

func TestWriteAtContractEdges(t *testing.T) {
	fs, w := openWritable(t, wpattern(100, 3))

	// An empty write is a no-op that succeeds: io.WriterAt says nothing else,
	// and refusing it would break callers that pass a zero-length buffer.
	if n, err := w.WriteAt(nil, 0); n != 0 || err != nil {
		t.Fatalf("WriteAt(empty) = %d, %v, want 0, nil", n, err)
	}
	// A negative offset is an error, never a panic and never a write.
	if n, err := w.WriteAt([]byte("x"), -1); n != 0 || err == nil {
		t.Fatalf("WriteAt(-1) = %d, %v, want an error", n, err)
	}
	// exFAT records a length in 64 bits, so the ceiling is not a field width
	// but the cluster heap: a write past what the volume can address is
	// refused before it allocates anything.
	if _, err := w.WriteAt([]byte("x"), int64(fs.maxChainBytes())); err == nil {
		t.Fatal("WriteAt past the heap capacity returned nil, want an error")
	}
	// ...including when the addition itself would overflow int64.
	if _, err := w.WriteAt(make([]byte, 8), 1<<62); err == nil {
		t.Fatal("WriteAt with an overflowing end returned nil, want an error")
	}
	if err := w.Truncate(-1); err == nil {
		t.Fatal("Truncate(-1) returned nil, want an error")
	}
	if err := w.Truncate(int64(fs.maxChainBytes()) + 1); err == nil {
		t.Fatal("Truncate past the heap capacity returned nil, want an error")
	}
}

func TestWriteAfterCloseIsRefused(t *testing.T) {
	_, w := openWritable(t, wpattern(100, 4))
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Close is idempotent, as the read path documents.
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := w.WriteAt([]byte("x"), 0); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("WriteAt after Close = %v, want os.ErrClosed", err)
	}
	if err := w.Truncate(0); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Truncate after Close = %v, want os.ErrClosed", err)
	}
	if err := w.Sync(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Sync after Close = %v, want os.ErrClosed", err)
	}
}

// syncRecorder is a diskRW that HAS a Sync, so the forwarding branch is
// exercised in both outcomes.
type syncRecorder struct {
	diskRW
	calls int
	err   error
}

func (s *syncRecorder) Sync() error { s.calls++; return s.err }

func TestSyncForwardsToTheBackingHandle(t *testing.T) {
	// The normal case: Open uses os.OpenFile, whose *os.File has Sync, so the
	// real image path really does reach fsync(2).
	fs, w := openWritable(t, wpattern(10, 5))
	if _, ok := fs.f.(interface{ Sync() error }); !ok {
		t.Fatal("a file-backed image's diskRW has no Sync — the FILE_SYNC promise would be empty")
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	rec := &syncRecorder{diskRW: fs.f}
	fs.f = rec
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if rec.calls != 1 {
		t.Fatalf("Sync forwarded %d times, want 1", rec.calls)
	}
	// A failing fsync must be reported, not swallowed: a server that answered
	// FILE_SYNC on it would be lying about durability.
	rec.err = errInjected
	if err := w.Sync(); !errors.Is(err, errInjected) {
		t.Fatalf("Sync = %v, want the backing error", err)
	}
}

// noSyncRW models a backing store with no Sync at all — a memory image, or a
// caller's own io.WriterAt. There is nothing to flush, so Sync must succeed
// rather than invent a failure.
type noSyncRW struct {
	r io.ReaderAt
	w io.WriterAt
}

func (n noSyncRW) ReadAt(p []byte, off int64) (int, error)  { return n.r.ReadAt(p, off) }
func (n noSyncRW) WriteAt(p []byte, off int64) (int, error) { return n.w.WriteAt(p, off) }
func (n noSyncRW) Close() error                             { return nil }

func TestSyncWithoutABackingSync(t *testing.T) {
	fs, w := openWritable(t, wpattern(10, 6))
	inner := fs.f
	fs.f = noSyncRW{r: inner, w: inner}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync with no backing Sync = %v, want nil", err)
	}
}

func TestWriteAtIOErrors(t *testing.T) {
	// A data-region write failure is reported with the bytes that did land.
	fs, w := openWritable(t, wpattern(4096*3, 7))
	wrapFault(fs).failDataWrites(fs)
	if _, err := w.WriteAt(wpattern(16, 8), 0); !errors.Is(err, errInjected) {
		t.Fatalf("WriteAt with a failing data write = %v, want the injected error", err)
	}

	// A FAT read failure during the allocation scan for a growing write.
	fs2, w2 := openWritable(t, wpattern(100, 9))
	wrapFault(fs2).failFATReads(fs2)
	if _, err := w2.WriteAt(wpattern(8, 10), 40000); !errors.Is(err, errInjected) {
		t.Fatalf("WriteAt with a failing FAT read = %v, want the injected error", err)
	}

	// A bitmap read failure during the same scan: the bitmap is consulted per
	// page of FAT, and a volume whose bitmap cannot be read must not fall
	// back to a FAT-only decision.
	fs3, w3 := openWritable(t, wpattern(100, 11))
	wrapFault(fs3).failBitmapReads(fs3)
	if _, err := w3.WriteAt(wpattern(8, 12), 40000); !errors.Is(err, errInjected) {
		t.Fatalf("WriteAt with a failing bitmap read = %v, want the injected error", err)
	}

	// A bitmap write failure while claiming the run: the clusters must be
	// given back, so the rollback runs.
	fs4, w4 := openWritable(t, wpattern(100, 13))
	wrapFault(fs4).failBitmapWrites(fs4)
	if _, err := w4.WriteAt(wpattern(8, 14), 40000); !errors.Is(err, errInjected) {
		t.Fatalf("WriteAt with a failing bitmap write = %v, want the injected error", err)
	}

	// A FAT write failure while claiming the run.
	fs5, w5 := openWritable(t, wpattern(100, 15))
	wrapFault(fs5).failFATWrites(fs5)
	if _, err := w5.WriteAt(wpattern(8, 16), 40000); !errors.Is(err, errInjected) {
		t.Fatalf("WriteAt with a failing FAT write = %v, want the injected error", err)
	}

	// A data write failure while zero-filling a freshly allocated cluster.
	fs6, w6 := openWritable(t, wpattern(100, 17))
	f6 := wrapFault(fs6)
	f6.failDataWrites(fs6)
	f6.writeRegionSkip = 0
	if _, err := w6.WriteAt(wpattern(8, 18), 40000); !errors.Is(err, errInjected) {
		t.Fatalf("WriteAt with a failing cluster zero-fill = %v, want the injected error", err)
	}
}

func TestTruncateIOErrors(t *testing.T) {
	// Shrink with the FAT unwritable: terminating the retained chain fails.
	fs, w := openWritable(t, wpattern(4096*6, 19))
	wrapFault(fs).failFATWrites(fs)
	if err := w.Truncate(4096); !errors.Is(err, errInjected) {
		t.Fatalf("Truncate(shrink) with a failing FAT write = %v", err)
	}

	// Freeing the dropped clusters fails after the retained chain has been
	// terminated: the first FAT write succeeds, the second does not.
	fs2, w2 := openWritable(t, wpattern(4096*6, 20))
	f2 := wrapFault(fs2)
	f2.failFATWrites(fs2)
	f2.writeRegionSkip = 1
	if err := w2.Truncate(4096); !errors.Is(err, errInjected) {
		t.Fatalf("Truncate(shrink) with the free pass failing = %v", err)
	}

	// Clearing a dropped cluster's bitmap bit fails.
	fs3, w3 := openWritable(t, wpattern(4096*6, 21))
	wrapFault(fs3).failBitmapWrites(fs3)
	if err := w3.Truncate(4096); !errors.Is(err, errInjected) {
		t.Fatalf("Truncate(shrink) with a failing bitmap write = %v", err)
	}

	// Shrink to a size whose last cluster has slack, with the data region
	// unwritable: zeroing the slack fails.
	fs4, w4 := openWritable(t, wpattern(4096*6, 22))
	wrapFault(fs4).failDataWrites(fs4)
	if err := w4.Truncate(4096 + 7); !errors.Is(err, errInjected) {
		t.Fatalf("Truncate(shrink, slack) with a failing data write = %v", err)
	}

	// Grow within the last cluster, with the data region unwritable: zeroing
	// the OLD cluster's slack fails.
	fs5, w5 := openWritable(t, wpattern(100, 23))
	wrapFault(fs5).failDataWrites(fs5)
	if err := w5.Truncate(200); !errors.Is(err, errInjected) {
		t.Fatalf("Truncate(grow in place) with a failing data write = %v", err)
	}
}

// TestGrowLinkFailures drives the two failure points that only exist once the
// new clusters have been claimed and zeroed: linking the run to itself, and
// linking it onto the file's existing tail. Both must roll back, so a failed
// grow does not leave the file pointing at a half-linked chain.
func TestGrowLinkFailures(t *testing.T) {
	// The run is claimed with one FAT write per cluster, then linked with one
	// per internal edge, then one for the tail: letting exactly the claims
	// through puts the fault on the first internal link.
	t.Run("internal-link", func(t *testing.T) {
		fs, w := openWritable(t, wpattern(100, 24))
		cs := int64(fs.info.ClusterSize())
		fault := wrapFault(fs)
		fault.failFATWrites(fs)
		fault.writeRegionSkip = 3 // three claims succeed
		if err := w.Truncate(100 + 3*cs); !errors.Is(err, errInjected) {
			t.Fatalf("grow with a failing internal link = %v, want the injected error", err)
		}
	})
	// A one-cluster run has no internal edges, so the second FAT write is the
	// link onto the existing tail.
	t.Run("tail-link", func(t *testing.T) {
		fs, w := openWritable(t, wpattern(100, 25))
		cs := int64(fs.info.ClusterSize())
		fault := wrapFault(fs)
		fault.failFATWrites(fs)
		fault.writeRegionSkip = 1 // the single claim succeeds
		if err := w.Truncate(cs + 1); !errors.Is(err, errInjected) {
			t.Fatalf("grow with a failing tail link = %v, want the injected error", err)
		}
	})
}

// TestMaterialiseChainFailures covers the conversion a NoFatChain file needs
// before its length can change. Each failure must leave the entry still saying
// NoFatChain, because that entry still describes the file correctly.
func TestMaterialiseChainFailures(t *testing.T) {
	mk := func(t *testing.T, name string, n int) (*exfatFS, filesystem.WritableFile) {
		t.Helper()
		fsIfc := formatImage(t, name)
		fs := fsIfc.(*exfatFS)
		cs := int(fs.info.ClusterSize())
		if err := fsIfc.WriteFile("/M.BIN", wpattern(cs*n, 0x44), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		makeNoFatChain(t, fs, "/M.BIN")
		f, err := fsIfc.(filesystem.Opener).OpenFile("/M.BIN")
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}
		t.Cleanup(func() { _ = f.Close() })
		return fs, probeWritable(t, f)
	}

	t.Run("internal-link", func(t *testing.T) {
		fs, w := mk(t, "mi.img", 3)
		wrapFault(fs).failFATWrites(fs)
		if err := w.Truncate(4096); !errors.Is(err, errInjected) {
			t.Fatalf("materialise with a failing link = %v", err)
		}
		entry, _, err := fs.resolvePath("/M.BIN")
		if err != nil {
			t.Fatalf("resolvePath: %v", err)
		}
		if entry.flags&exfatFlagNoFatChain == 0 {
			t.Fatal("a failed conversion cleared NoFatChain, so the file now describes a chain it has not got")
		}
	})

	t.Run("terminator", func(t *testing.T) {
		// Two clusters: one internal link succeeds, the terminator fails.
		fs, w := mk(t, "mt.img", 2)
		fault := wrapFault(fs)
		fault.failFATWrites(fs)
		fault.writeRegionSkip = 1
		if err := w.Truncate(4096); !errors.Is(err, errInjected) {
			t.Fatalf("materialise with a failing terminator = %v", err)
		}
	})

	t.Run("entry-gone", func(t *testing.T) {
		// The conversion has to rewrite the entry to clear the flag; if the
		// entry is gone the whole resize must fail rather than half-apply.
		fs, w := mk(t, "me.img", 2)
		if err := fs.DeleteFile("/M.BIN"); err != nil {
			t.Fatalf("DeleteFile: %v", err)
		}
		if err := w.Truncate(4096); err == nil {
			t.Fatal("materialise with the entry deleted returned nil, want an error")
		}
	})
}

// TestShrinkToEmptyAndBackFromEmpty covers the two allocation-boundary cases:
// a file with no first cluster at all, and one grown from that state.
func TestShrinkToEmptyAndBackFromEmpty(t *testing.T) {
	fs, w := openWritable(t, wpattern(9000, 26))
	if err := w.Truncate(0); err != nil {
		t.Fatalf("Truncate(0): %v", err)
	}
	if w.Size() != 0 {
		t.Fatalf("Size() = %d after Truncate(0)", w.Size())
	}
	// exFAT spells "empty" as first-cluster zero with AllocationPossible
	// clear; a file left pointing at a freed cluster would be corruption a
	// later ReadFile would surface.
	entry, _, err := fs.resolvePath("/W.BIN")
	if err != nil {
		t.Fatalf("resolvePath: %v", err)
	}
	if entry.cluster != 0 {
		t.Fatalf("emptied file still points at cluster %d, want 0", entry.cluster)
	}
	if entry.flags != 0 {
		t.Fatalf("emptied file has GeneralSecondaryFlags %#02x, want 0", entry.flags)
	}
	// ...and growing from empty must give it a head again.
	back := wpattern(5000, 27)
	if _, err := w.WriteAt(back, 0); err != nil {
		t.Fatalf("WriteAt from empty: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	checkReadPathsAgree(t, fs, "/W.BIN", back)
}

// TestWriteAtWhenTheDirectoryEntryIsGone covers the failure a positional write
// cannot avoid: exFAT keeps the length in the entry set, so if the entry has
// been removed underneath the File, the size cannot be recorded.
func TestWriteAtWhenTheDirectoryEntryIsGone(t *testing.T) {
	fs, w := openWritable(t, wpattern(100, 28))
	if err := fs.DeleteFile("/W.BIN"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if _, err := w.WriteAt(wpattern(8, 29), 40000); err == nil {
		t.Fatal("WriteAt after the entry was deleted returned nil, want an error")
	}
}

func TestSetStreamExtentRejects(t *testing.T) {
	fs, _ := openWritable(t, wpattern(10, 30))
	if err := fs.MkDir("/dir", 0o755); err != nil {
		t.Fatalf("MkDir: %v", err)
	}
	if err := fs.setStreamExtent("no-leading-slash", 2, 10); err == nil {
		t.Fatal("setStreamExtent on a relative path returned nil, want an error")
	}
	// A trailing slash leaves an empty final component: there is no entry to
	// patch, and patching the directory's own would be wrong.
	if err := fs.setStreamExtent("/dir/", 2, 10); err == nil {
		t.Fatal("setStreamExtent on a trailing-slash path returned nil, want an error")
	}
	if err := fs.setStreamExtent("/NOPE.BIN", 2, 10); err == nil {
		t.Fatal("setStreamExtent on a missing entry returned nil, want an error")
	}
	// A directory read failure on the way to the entry.
	fault := wrapFault(fs)
	fault.failReadLo, fault.failReadHi = 0, 1<<62
	if err := fs.setStreamExtent("/W.BIN", 2, 10); !errors.Is(err, errInjected) {
		t.Fatalf("setStreamExtent with an unreadable directory = %v", err)
	}
}

func TestAllocClusterRunEdges(t *testing.T) {
	fs, _ := openWritable(t, wpattern(10, 31))
	// Asking for nothing is not an error; it is the loop bound at zero.
	run, err := fs.allocClusterRun(0)
	if run != nil || err != nil {
		t.Fatalf("allocClusterRun(0) = %v, %v, want nil, nil", run, err)
	}
	// A run larger than the whole volume can supply must fail, not return
	// short: a short run would be linked into a file that then claims bytes
	// no cluster holds.
	if _, err := fs.allocClusterRun(1 << 30); err == nil {
		t.Fatal("allocClusterRun beyond the volume returned nil, want an error")
	}
	// A run spanning more than one page of FAT, to cross the read boundary.
	big, err := fs.allocClusterRun(clustersPerScan + 5)
	if err != nil {
		t.Fatalf("allocClusterRun across a page boundary: %v", err)
	}
	if len(big) != clustersPerScan+5 {
		t.Fatalf("allocClusterRun returned %d clusters", len(big))
	}
	for i := 1; i < len(big); i++ {
		if big[i] <= big[i-1] {
			t.Fatalf("allocClusterRun returned an out-of-order run at %d", i)
		}
	}
}

// TestBitmapBitsFromShortAndAbsent covers the two ways the Allocation Bitmap
// can decline to answer: it does not cover the clusters being scanned, or the
// volume has no bitmap at all. Both mean "ask the FAT", which is what this
// driver did everywhere before the bitmap was consulted.
func TestBitmapBitsFromShortAndAbsent(t *testing.T) {
	fs, _ := openWritable(t, wpattern(10, 32))
	// Pretend the bitmap covers only the first 32 clusters. The first scan
	// pass is then partly covered (the clamp) and later passes not at all.
	fs.bitmapLength = 4
	run, err := fs.allocClusterRun(clustersPerScan + 5)
	if err != nil {
		t.Fatalf("allocClusterRun with a short bitmap: %v", err)
	}
	if len(run) != clustersPerScan+5 {
		t.Fatalf("allocClusterRun returned %d clusters", len(run))
	}

	// No bitmap at all.
	fs.bitmapCluster = 0
	if _, err := fs.allocClusterRun(4); err != nil {
		t.Fatalf("allocClusterRun with no bitmap: %v", err)
	}
	if bits, err := fs.bitmapBitsFrom(2, 8); bits != nil || err != nil {
		t.Fatalf("bitmapBitsFrom with no bitmap = %v, %v, want nil, nil", bits, err)
	}
}

// TestAllocClusterBitmapWriteFails covers the one branch allocCluster still
// owns: it claims the cluster in the bitmap before handing it over.
func TestAllocClusterBitmapWriteFails(t *testing.T) {
	fs, _ := openWritable(t, wpattern(10, 33))
	wrapFault(fs).failBitmapWrites(fs)
	if _, err := fs.allocCluster(); !errors.Is(err, errInjected) {
		t.Fatalf("allocCluster with a failing bitmap write = %v", err)
	}
}

// TestWriteAtNoFreeClusters: a grow that cannot be satisfied must fail the
// write outright rather than write part of it.
func TestWriteAtNoFreeClusters(t *testing.T) {
	fsIfc := formatImage(t, "full.img")
	fs := fsIfc.(*exfatFS)
	if err := fsIfc.WriteFile("/S.BIN", wpattern(100, 34), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	f, err := fsIfc.(filesystem.Opener).OpenFile("/S.BIN")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	w := probeWritable(t, f)
	// Claim every remaining cluster, in the largest runs the volume will
	// give, until none is left. Only then does a grow have nowhere to go.
	claimed := 0
	for n := int(fs.info.ClusterCount) + 2; n > 0; n /= 2 {
		for {
			run, err := fs.allocClusterRun(n)
			if err != nil {
				break
			}
			for _, c := range run {
				if err := fs.setFATEntry(c, exfatEndOfChain); err != nil {
					t.Fatalf("setFATEntry: %v", err)
				}
			}
			claimed += len(run)
		}
	}
	if claimed == 0 {
		t.Fatal("claimed no clusters — the volume was already full, so the test proves nothing")
	}
	t.Logf("claimed %d clusters to fill the volume", claimed)
	if _, err := w.WriteAt(wpattern(8, 35), 1_500_000); err == nil {
		t.Fatal("WriteAt on a full volume returned nil, want an error")
	}
}

// TestContiguousClustersClamps pins the bounds on the NoFatChain walk. The
// declared length is attacker-controlled exactly as it is for a chain, so it is
// clamped to the heap's capacity and to the clusters that exist past the start.
func TestContiguousClustersClamps(t *testing.T) {
	fs, _ := openWritable(t, wpattern(10, 36))
	cs := int64(fs.info.ClusterSize())

	if c, n := fs.contiguousClusters(0, 100); c != nil || n != 0 {
		t.Fatalf("contiguousClusters(0) = %v, %d, want nil, 0", c, n)
	}
	if c, n := fs.contiguousClusters(fs.info.ClusterCount+2, 100); c != nil || n != 0 {
		t.Fatalf("contiguousClusters(past the heap) = %v, %d, want nil, 0", c, n)
	}
	// A forged length far past the volume yields the heap capacity, not an
	// enormous allocation.
	if _, n := fs.contiguousClusters(2, 1<<62); n != int64(fs.maxChainBytes()) {
		t.Fatalf("contiguousClusters(2, 2^62) covered %d, want the heap cap %d", n, fs.maxChainBytes())
	}
	// Starting at the LAST cluster, only one cluster can be addressed however
	// long the entry claims to be.
	if _, n := fs.contiguousClusters(fs.info.ClusterCount+1, fs.maxChainBytes()); n != cs {
		t.Fatalf("contiguousClusters(last cluster) covered %d, want %d", n, cs)
	}
}

// TestReadContiguousRunIOError: a read failure inside a NoFatChain run is
// reported, not silently turned into a short file.
func TestReadContiguousRunIOError(t *testing.T) {
	fsIfc := formatImage(t, "cerr.img")
	fs := fsIfc.(*exfatFS)
	cs := int(fs.info.ClusterSize())
	if err := fsIfc.WriteFile("/R.BIN", wpattern(cs*3, 0x66), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	makeNoFatChain(t, fs, "/R.BIN")
	wrapFault(fs).failDataWrites(fs) // arm nothing on writes; reads next
	fault := fs.f.(*faultRW)
	fault.failWriteLo, fault.failWriteHi = 0, 0
	fault.failReadLo, fault.failReadHi = userDataRegion(fs)
	if _, err := fsIfc.ReadFile("/R.BIN"); !errors.Is(err, errInjected) {
		t.Fatalf("ReadFile over a failing contiguous run = %v, want the injected error", err)
	}
}

// TestAllocScanIsBoundedByTheFATsOwnLength covers the clamp in
// allocClusterRun, and the reason it is there.
//
// The scan's upper bound is the smaller of two numbers that a trustworthy
// image agrees on and a forged one does not: ClusterCount + 2, taken from the
// boot sector, and however many 32-bit entries the FAT's declared length can
// actually hold. A boot sector claiming more clusters than its FAT describes
// would otherwise make the scan read PAST THE FIRST FAT — into its mirror on a
// two-FAT volume, or past the end of the image — and interpret whatever it
// found there as free-cluster markers, handing the caller cluster numbers the
// volume has no room for.
//
// The forgery is done in memory, on the parsed Info, rather than by rewriting
// the boot sector: what is under test is the clamp, and going through Open
// would only prove the boot-sector validator rejects the image first, which is
// a different guarantee in a different function.
func TestAllocScanIsBoundedByTheFATsOwnLength(t *testing.T) {
	fsIfc := formatImage(t, "bounded.img")
	fs := fsIfc.(*exfatFS)

	fatEntries := uint32(fs.info.FATLength * fs.info.BytesPerSector() / 4)
	if fs.info.ClusterCount+2 > fatEntries {
		t.Skip("this geometry already has more clusters than FAT entries; nothing to clamp")
	}
	// Claim a FAT ten times longer than the volume has, which is what an
	// attacker-supplied boot sector looks like.
	fs.info.ClusterCount = fatEntries * 10

	run, err := fs.allocClusterRun(4)
	if err != nil {
		t.Fatalf("allocClusterRun on a volume whose boot sector over-claims: %v", err)
	}
	if len(run) != 4 {
		t.Fatalf("allocClusterRun returned %d clusters, want 4", len(run))
	}
	for _, c := range run {
		if uint32(c) >= fatEntries {
			t.Fatalf("cluster %d is past the %d entries the FAT can describe: the scan was not clamped", c, fatEntries)
		}
	}
}
