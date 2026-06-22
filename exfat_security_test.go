package filesystem_exfat

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-volumes/gpt"
	"github.com/go-volumes/safeio"
)

// The tests in this file exercise the hardening added against malicious or
// corrupt exFAT images: a self-referential / cyclic FAT cluster chain, an
// over-long declared data length (2^63), and a hostile GPT partition-entry
// size. None of them must panic, OOB-read, integer-overflow into a bad
// allocation, loop forever, or OOM — every vector must surface a graceful
// error or a bounded result.

// buildExFATImage returns a 1 MiB image plus its decoded Info, with the root
// directory cluster's FAT entry pre-set to EOF so allocCluster never steals
// it. The caller customises the FAT and the root directory through the two
// callbacks.
func buildExFATImage(t *testing.T, setupFAT func(image []byte, info Info), setupRoot func(root []byte, info Info)) ([]byte, Info) {
	t.Helper()
	image := make([]byte, 1024*1024)
	boot := defaultExFATBootSector()
	copy(image, boot)
	info, err := readInfo(bytes.NewReader(boot), 0)
	if err != nil {
		t.Fatalf("readInfo: %v", err)
	}
	fatBase := int(info.FATOffsetBytes(0))
	binary.LittleEndian.PutUint32(image[fatBase+int(info.RootDirectoryCluster)*4:], 0xFFFFFFFF)
	if setupFAT != nil {
		setupFAT(image, info)
	}
	if setupRoot != nil {
		setupRoot(image[int(info.RootDirOffset(0)):], info)
	}
	return image, info
}

// TestReadClusterChainCycle proves a self-referential FAT chain (cluster 3
// points at itself) is rejected with safeio.ErrCycle instead of looping
// forever and OOMing.
func TestReadClusterChainCycle(t *testing.T) {
	image, info := buildExFATImage(t, func(image []byte, info Info) {
		fatBase := int(info.FATOffsetBytes(0))
		binary.LittleEndian.PutUint32(image[fatBase+3*4:], 3) // FAT[3] = 3
	}, nil)
	fs := newMockFSWithErrors(image, info, 0, nil, nil)
	// Request a large size so the len(buf) >= size break cannot fire first.
	_, err := fs.readClusterChain(3, 1<<40)
	if err == nil {
		t.Fatal("readClusterChain(cyclic) error = nil, want cycle error")
	}
	if !errors.Is(err, safeio.ErrCycle) {
		t.Fatalf("readClusterChain(cyclic) error = %v, want ErrCycle", err)
	}
}

// TestReadClusterChainBoundedByClusterCount proves a long forward-walking
// (acyclic) chain is bounded by the heap-capacity size clamp: the result is
// capped at ClusterCount*ClusterSize and the walk terminates, so the visited
// set cannot grow without bound. With ClusterCount=2 the result is exactly
// two clusters even though the chain keeps advancing.
func TestReadClusterChainBoundedByClusterCount(t *testing.T) {
	image, info := buildExFATImage(t, func(image []byte, info Info) {
		fatBase := int(info.FATOffsetBytes(0))
		// 2 -> 3 -> 4 -> 5 -> ... walking forward, never EOF.
		for c := uint32(2); c < 20; c++ {
			binary.LittleEndian.PutUint32(image[fatBase+int(c)*4:], c+1)
		}
	}, nil)
	info.ClusterCount = 2 // cap = 2 clusters
	fs := newMockFSWithErrors(image, info, 0, nil, nil)
	buf, err := fs.readClusterChain(2, 1<<40)
	if err != nil {
		t.Fatalf("readClusterChain(long chain) error = %v, want nil (bounded)", err)
	}
	if uint64(len(buf)) != fs.maxChainBytes() {
		t.Fatalf("readClusterChain(long chain) returned %d bytes, want cap %d", len(buf), fs.maxChainBytes())
	}
}

// TestReadClusterChainHugeDataLength proves an attacker dataLength of 2^63 is
// clamped to the heap capacity instead of triggering a multi-exabyte
// allocation. The returned slice is bounded by ClusterCount*ClusterSize.
func TestReadClusterChainHugeDataLength(t *testing.T) {
	image, info := buildExFATImage(t, func(image []byte, info Info) {
		fatBase := int(info.FATOffsetBytes(0))
		binary.LittleEndian.PutUint32(image[fatBase+3*4:], 0xFFFFFFFF) // EOF
	}, nil)
	fs := newMockFSWithErrors(image, info, 0, nil, nil)
	const evil = uint64(1) << 63
	buf, err := fs.readClusterChain(3, evil)
	if err != nil {
		t.Fatalf("readClusterChain(2^63) error = %v, want nil", err)
	}
	if uint64(len(buf)) > fs.maxChainBytes() {
		t.Fatalf("readClusterChain(2^63) returned %d bytes, exceeds heap cap %d", len(buf), fs.maxChainBytes())
	}
	// Single EOF cluster => exactly one cluster of data.
	if uint64(len(buf)) != info.ClusterSize() {
		t.Fatalf("readClusterChain(2^63) returned %d bytes, want %d", len(buf), info.ClusterSize())
	}
}

// TestReadClusterChainOverLargeSizeClamp drives the MakeBytes-rejection path
// with a size that merely exceeds the heap cap (not 2^63), confirming the
// clamp branch.
func TestReadClusterChainOverLargeSizeClamp(t *testing.T) {
	image, info := buildExFATImage(t, func(image []byte, info Info) {
		fatBase := int(info.FATOffsetBytes(0))
		binary.LittleEndian.PutUint32(image[fatBase+3*4:], 0xFFFFFFFF)
	}, nil)
	fs := newMockFSWithErrors(image, info, 0, nil, nil)
	cap := fs.maxChainBytes()
	buf, err := fs.readClusterChain(3, cap+info.ClusterSize())
	if err != nil {
		t.Fatalf("readClusterChain(>cap) error = %v, want nil", err)
	}
	if uint64(len(buf)) > cap {
		t.Fatalf("readClusterChain(>cap) returned %d bytes, exceeds cap %d", len(buf), cap)
	}
}

// TestFreeChainCycle proves freeChain rejects a self-referential FAT chain.
func TestFreeChainCycle(t *testing.T) {
	image, info := buildExFATImage(t, func(image []byte, info Info) {
		fatBase := int(info.FATOffsetBytes(0))
		binary.LittleEndian.PutUint32(image[fatBase+3*4:], 3) // FAT[3] = 3
	}, nil)
	fs := newMockFSWithErrors(image, info, 0, nil, nil)
	err := fs.freeChain(3)
	if err == nil {
		t.Fatal("freeChain(cyclic) error = nil, want cycle error")
	}
	if !errors.Is(err, safeio.ErrCycle) {
		t.Fatalf("freeChain(cyclic) error = %v, want ErrCycle", err)
	}
}

// TestFreeChainLoopLimit proves freeChain is bounded by its LoopGuard on a
// long forward-walking chain.
func TestFreeChainLoopLimit(t *testing.T) {
	image, info := buildExFATImage(t, func(image []byte, info Info) {
		fatBase := int(info.FATOffsetBytes(0))
		for c := uint32(2); c < 20; c++ {
			binary.LittleEndian.PutUint32(image[fatBase+int(c)*4:], c+1)
		}
	}, nil)
	info.ClusterCount = 2
	fs := newMockFSWithErrors(image, info, 0, nil, nil)
	err := fs.freeChain(2)
	if err == nil {
		t.Fatal("freeChain(long chain) error = nil, want loop-limit error")
	}
	if !errors.Is(err, safeio.ErrLoopLimit) {
		t.Fatalf("freeChain(long chain) error = %v, want ErrLoopLimit", err)
	}
}

// TestFreeChainSetBitmapError proves that a bitmap-write failure encountered
// while freeing a (guarded) chain is surfaced as an error rather than
// swallowed — exercising the setBitmapBit error branch inside freeChain.
func TestFreeChainSetBitmapError(t *testing.T) {
	image, info := buildExFATImage(t, func(image []byte, info Info) {
		fatBase := int(info.FATOffsetBytes(0))
		binary.LittleEndian.PutUint32(image[fatBase+3*4:], 0xFFFFFFFF) // EOF
	}, nil)
	// Configure a bitmap so setBitmapBit actually touches the disk.
	const bitmapCluster = uint32(10)
	dataBase := info.ClusterHeapOffsetBytes(0)
	bitmapOff := dataBase + int64(bitmapCluster-2)*int64(info.ClusterSize())
	fs := newMockFSWithErrors(image, info, 0, nil, func(off int64) error {
		if off == bitmapOff {
			return errors.New("bitmap write error")
		}
		return nil
	})
	fs.bitmapCluster = bitmapCluster
	fs.bitmapLength = uint64(info.ClusterSize())
	if err := fs.freeChain(3); err == nil {
		t.Fatal("freeChain setBitmapBit write error = nil, want error")
	}
}

// TestWriteDirBufCycle proves writeDirBuf rejects a cyclic directory FAT
// chain. The buffer is larger than one cluster so the walk must follow the
// FAT past the first cluster, where the self-reference is caught.
func TestWriteDirBufCycle(t *testing.T) {
	image, info := buildExFATImage(t, func(image []byte, info Info) {
		fatBase := int(info.FATOffsetBytes(0))
		binary.LittleEndian.PutUint32(image[fatBase+3*4:], 3) // FAT[3] = 3
	}, nil)
	fs := newMockFSWithErrors(image, info, 0, nil, nil)
	buf := make([]byte, int(info.ClusterSize())*2) // forces a second hop
	err := fs.writeDirBuf(3, buf)
	if err == nil {
		t.Fatal("writeDirBuf(cyclic) error = nil, want cycle error")
	}
	if !errors.Is(err, safeio.ErrCycle) {
		t.Fatalf("writeDirBuf(cyclic) error = %v, want ErrCycle", err)
	}
}

// TestWriteDirBufLoopLimit proves writeDirBuf's LoopGuard bounds a long
// forward-walking directory chain.
func TestWriteDirBufLoopLimit(t *testing.T) {
	image, info := buildExFATImage(t, func(image []byte, info Info) {
		fatBase := int(info.FATOffsetBytes(0))
		for c := uint32(2); c < 20; c++ {
			binary.LittleEndian.PutUint32(image[fatBase+int(c)*4:], c+1)
		}
	}, nil)
	info.ClusterCount = 2
	fs := newMockFSWithErrors(image, info, 0, nil, nil)
	buf := make([]byte, int(info.ClusterSize())*10)
	err := fs.writeDirBuf(2, buf)
	if err == nil {
		t.Fatal("writeDirBuf(long chain) error = nil, want loop-limit error")
	}
	if !errors.Is(err, safeio.ErrLoopLimit) {
		t.Fatalf("writeDirBuf(long chain) error = %v, want ErrLoopLimit", err)
	}
}

// TestWriteDirBufInvalidStartCluster proves writeDirBuf rejects a directory
// whose start cluster is below the first data cluster (an out-of-range FAT
// pointer) instead of computing a negative on-disk offset.
func TestWriteDirBufInvalidStartCluster(t *testing.T) {
	image, info := buildExFATImage(t, nil, nil)
	fs := newMockFSWithErrors(image, info, 0, nil, nil)
	buf := make([]byte, int(info.ClusterSize()))
	if err := fs.writeDirBuf(1, buf); err == nil {
		t.Fatal("writeDirBuf(cluster<2) error = nil, want invalid-cluster error")
	}
}

// TestGPTHostileEntrySize proves a GPT header advertising a 0xFFFFFFFF
// partition-entry size is rejected by the hardened go-volumes/gpt parser
// (ErrMalformed) rather than driving a ~4 GiB allocation and an int64 offset
// overflow.
func TestGPTHostileEntrySize(t *testing.T) {
	image := make([]byte, 64*sectorSize)
	writeGPTHeaderOnly(image, 2, 0xFFFFFFFF, 1)
	_, err := partitionOffset(bytes.NewReader(image), -1)
	if err == nil {
		t.Fatal("partitionOffset(entrySize=0xFFFFFFFF) error = nil, want malformed error")
	}
	if !errors.Is(err, gpt.ErrMalformed) {
		t.Fatalf("partitionOffset(entrySize=0xFFFFFFFF) error = %v, want ErrMalformed", err)
	}
}

// TestGPTHostileNumParts proves a GPT header advertising 0xFFFFFFFF partition
// entries is capped instead of allocating a huge table.
func TestGPTHostileNumParts(t *testing.T) {
	image := make([]byte, 64*sectorSize)
	writeGPTHeaderOnly(image, 2, 128, 0xFFFFFFFF)
	_, err := partitionOffset(bytes.NewReader(image), -1)
	if err == nil {
		t.Fatal("partitionOffset(numParts=0xFFFFFFFF) error = nil, want malformed error")
	}
	if !errors.Is(err, gpt.ErrMalformed) {
		t.Fatalf("partitionOffset(numParts=0xFFFFFFFF) error = %v, want ErrMalformed", err)
	}
}

// TestReaderSizeFromStat exercises the *os.File (Stat) branch of readerSize:
// a real file on disk reports its size so the gpt parser validates against
// the true device extent.
func TestReaderSizeFromStat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sized.img")
	if err := os.WriteFile(path, make([]byte, 4096), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	if got := readerSize(f); got != 4096 {
		t.Fatalf("readerSize(*os.File) = %d, want 4096", got)
	}
}

// TestReaderSizeUnknown exercises the fallback branch of readerSize: a reader
// that exposes neither Size() nor Stat() yields math.MaxInt64 so gpt still
// applies its allocation/overflow caps.
func TestReaderSizeUnknown(t *testing.T) {
	r := plainReaderAt{data: make([]byte, 16)}
	if got := readerSize(r); got <= 0 {
		t.Fatalf("readerSize(plain) = %d, want positive fallback", got)
	}
}

// plainReaderAt implements only io.ReaderAt — no Size() and no Stat() — so it
// forces readerSize down its fallback path.
type plainReaderAt struct{ data []byte }

func (p plainReaderAt) ReadAt(b []byte, off int64) (int, error) {
	if off >= int64(len(p.data)) {
		return 0, nil
	}
	return copy(b, p.data[off:]), nil
}

// validExFATImageBytes returns a minimal but structurally valid bare exFAT
// image (boot sector + an empty root directory cluster) for use as a fuzz
// seed.
func validExFATImageBytes() []byte {
	image := make([]byte, 1024*1024)
	boot := defaultExFATBootSector()
	copy(image, boot)
	info, err := readInfo(bytes.NewReader(boot), 0)
	if err != nil {
		panic(err)
	}
	fatBase := int(info.FATOffsetBytes(0))
	binary.LittleEndian.PutUint32(image[fatBase+int(info.RootDirectoryCluster)*4:], 0xFFFFFFFF)
	// Root directory cluster starts with an end-of-directory marker (0x00),
	// which is already the zero value, so nothing more to write.
	return image
}

// cyclicChainSeed returns a valid image whose root directory references a file
// at cluster 3 whose FAT entry points back at itself (the canonical FAT
// cycle attack).
func cyclicChainSeed() []byte {
	image := validExFATImageBytes()
	boot := defaultExFATBootSector()
	info, _ := readInfo(bytes.NewReader(boot), 0)
	fatBase := int(info.FATOffsetBytes(0))
	binary.LittleEndian.PutUint32(image[fatBase+3*4:], 3) // FAT[3] = 3 (cycle)
	root := image[int(info.RootDirOffset(0)):]
	// A file entry set "loop.bin" at cluster 3 with a large declared length.
	writeExFATEntrySetWithAttr(root, "loop.bin", 3, 0x20, 1<<63)
	return image
}

// hugeLengthSeed returns a valid image whose root directory references a file
// with a declared data length of 2^63 (the over-allocation attack).
func hugeLengthSeed() []byte {
	image := validExFATImageBytes()
	boot := defaultExFATBootSector()
	info, _ := readInfo(bytes.NewReader(boot), 0)
	fatBase := int(info.FATOffsetBytes(0))
	binary.LittleEndian.PutUint32(image[fatBase+3*4:], 0xFFFFFFFF) // EOF
	root := image[int(info.RootDirOffset(0)):]
	writeExFATEntrySetWithAttr(root, "huge.bin", 3, 0x20, 1<<63)
	return image
}

// gptHostileSeed returns an image fronted by a GPT header whose partition
// entry size is 0xFFFFFFFF.
func gptHostileSeed() []byte {
	image := make([]byte, 64*sectorSize)
	writeGPTHeaderOnly(image, 2, 0xFFFFFFFF, 0xFFFFFFFF)
	return image
}

// exerciseImage opens an image from raw bytes and walks every read path that
// touches attacker-controlled chains/lengths/offsets. It must never panic and
// must always return (never hang). Errors are expected and ignored; the point
// is that no input crashes the process.
func exerciseImage(t *testing.T, data []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fuzz.img")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// Try both auto-detect and an explicit partition index so the GPT/MBR
	// path is exercised on every input.
	for _, idx := range []int{-1, 0} {
		fs, err := Open(path, idx)
		if err != nil {
			continue
		}
		// Walk the filesystem. Each call internally follows cluster chains
		// and honours declared lengths; all are hardened.
		_, _ = fs.Stat("/")
		entries, err := fs.ListDir("/")
		if err == nil {
			for _, e := range entries {
				name := "/" + e.Name()
				_, _ = fs.Stat(name)
				_, _ = fs.ReadFile(name)
				_, _ = fs.ListDir(name)
			}
		}
		_ = fs.Close()
	}
}

// FuzzOpenAndWalk feeds arbitrary bytes (seeded with the exact attack vectors)
// through Open + the read API. The contract: no input may panic, OOB-read,
// overflow into a bad allocation, loop forever, or OOM — only graceful errors.
// The seed corpus runs under plain `go test`.
func FuzzOpenAndWalk(f *testing.F) {
	f.Add(validExFATImageBytes())
	f.Add(cyclicChainSeed())
	f.Add(hugeLengthSeed())
	f.Add(gptHostileSeed())
	f.Add([]byte{})
	f.Add(make([]byte, sectorSize))
	f.Fuzz(func(t *testing.T, data []byte) {
		exerciseImage(t, data)
	})
}
