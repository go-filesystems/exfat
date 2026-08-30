package filesystem_exfat

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sync/atomic"

	filesystem "github.com/go-filesystems/interface"
	"github.com/go-volumes/safeio"
)

// Verify implementation of the optional read-at-an-offset interface.
var _ filesystem.Opener = (*exfatFS)(nil)

// exfatFile is an open regular file on an exFAT volume, backing
// filesystem.File.
//
// exFAT, like FAT before it, has no extents: a file is a singly-linked list of
// fixed-size clusters threaded through the FAT, and the only way to learn where
// byte N lives is to walk that list from the start. Walking it on every ReadAt
// would make random access quadratic, so the walk happens exactly once, at
// OpenFile, and what it materialises is the CLUSTER NUMBERS — not the data. A
// 4 GiB file with 32 KiB clusters costs half a megabyte of uint32s to address,
// instead of 4 GiB of contents to read.
//
// The walk is the same one readClusterChain performs, with the same safeio
// guard against a forged cyclic chain and the same clamp of an
// attacker-controlled dataLength to the cluster heap's real capacity, and reads
// go through the same *exfatFS block layer. Nothing here bypasses either.
//
// Every field is written once during OpenFile and only read afterwards, so
// concurrent ReadAt calls need no synchronisation of their own — as
// io.ReaderAt requires. The one mutable field, closed, is atomic.
type exfatFile struct {
	fs *exfatFS
	// clusters holds the file's chain in order: clusters[i] is the cluster
	// number holding bytes [i*clusterSize, (i+1)*clusterSize).
	clusters []uint32
	// size is the readable length in bytes: the directory entry's dataLength
	// clamped to what the chain can actually address, so it always equals
	// len(fs.ReadFile(path)) — a truncated or forged chain shortens the file
	// rather than inventing zeros.
	size        int64
	clusterSize int64
	dataBase    int64
	closed      atomic.Bool
}

var _ filesystem.File = (*exfatFile)(nil)

// OpenFile opens the regular file at path for random access.
//
// It resolves the path and walks the FAT chain to build the cluster list, but
// reads none of the file's contents: the cost is one FAT entry read per
// cluster, not one data cluster read per cluster. Directories and "/" are
// rejected with the same message ReadFile uses for them.
func (fs *exfatFS) OpenFile(path string) (filesystem.File, error) {
	if path == "/" {
		return nil, fmt.Errorf("exfat: %q is not a regular file", path)
	}
	entry, _, err := fs.resolvePath(path)
	if err != nil {
		return nil, err
	}
	if entry.attr&exfatAttrDir != 0 {
		return nil, fmt.Errorf("exfat: %q is not a regular file", path)
	}
	clusters, size, err := fs.chainClusters(entry.cluster, entry.size)
	if err != nil {
		return nil, err
	}
	return &exfatFile{
		fs:          fs,
		clusters:    clusters,
		size:        size,
		clusterSize: int64(fs.info.ClusterSize()),
		dataBase:    fs.info.ClusterHeapOffsetBytes(fs.partOffset),
	}, nil
}

// chainClusters walks the FAT chain starting at start and returns the cluster
// numbers covering up to size bytes, together with the number of bytes those
// clusters actually address.
//
// It mirrors readClusterChain's loop exactly — same termination conditions,
// same safeio.VisitSet rejecting a cyclic chain, same clamp of a forged
// dataLength to the heap capacity — but reads only FAT entries, never data.
// Keeping the two walks in step is what guarantees OpenFile+ReadAt and ReadFile
// agree byte for byte, including on images whose chain ends before dataLength
// says it should.
func (fs *exfatFS) chainClusters(start uint32, size uint64) ([]uint32, int64, error) {
	if start == 0 {
		return nil, 0, nil
	}
	clusterSize := int64(fs.info.ClusterSize())
	fatBase := fs.info.FATOffsetBytes(fs.partOffset)

	capBytes := fs.maxChainBytes()
	if _, err := safeio.MakeBytes(int64(size), int64(capBytes)); err != nil {
		size = capBytes
	}

	var clusters []uint32
	var covered int64
	// The walk appends one cluster per iteration and size is clamped to the
	// heap capacity, so it terminates after at most ClusterCount iterations
	// even on a maximal acyclic chain; the VisitSet bounds a cyclic one.
	var seen safeio.VisitSet
	cluster := start
	for {
		if cluster < 2 || cluster >= 0xFFFFFFF7 {
			break
		}
		if uint64(covered) >= size {
			break
		}
		if err := seen.Check(uint64(cluster)); err != nil {
			return nil, 0, fmt.Errorf("exfat: cluster chain from %d: %w", start, err)
		}
		clusters = append(clusters, cluster)
		covered += clusterSize
		var nextEntry [4]byte
		if _, err := fs.f.ReadAt(nextEntry[:], fatBase+int64(cluster)*4); err != nil {
			return nil, 0, fmt.Errorf("exfat: read FAT entry for cluster %d: %w", cluster, err)
		}
		next := binary.LittleEndian.Uint32(nextEntry[:])
		if next >= 0xFFFFFFF8 {
			break
		}
		cluster = next
	}
	if covered > int64(size) {
		covered = int64(size)
	}
	return clusters, covered, nil
}

// Size returns the file's readable length in bytes, taken from the directory
// entry read at OpenFile time and clamped to what the chain addresses. No I/O.
func (f *exfatFile) Size() int64 { return f.size }

// Close releases the File. exFAT files hold no per-file handle — the volume's
// single descriptor stays owned by the Filesystem — so Close only marks the
// File unusable, which turns a use-after-close into a clear os.ErrClosed
// instead of a silent read of stale cluster numbers. It is idempotent.
func (f *exfatFile) Close() error {
	f.closed.Store(true)
	return nil
}

// ReadAt implements io.ReaderAt to the letter, which is the contract every
// generic consumer (io.SectionReader above all) silently depends on:
//
//   - it fills p completely and returns a nil error whenever the bytes exist;
//   - it returns n < len(p) only together with a non-nil error;
//   - a read that runs into the end of the file returns io.EOF, with the
//     bytes it did get, and an offset at or past Size() returns 0, io.EOF.
//
// The loop translates a byte offset into (cluster index, offset within
// cluster) by division — possible only because the chain was resolved up
// front — and issues one read per cluster crossed, never more than the caller
// asked for. Reads go through fs.f, the same block layer every other path in
// this package uses; concurrent calls are safe because nothing here mutates
// shared state and *os.File.ReadAt is itself concurrent-safe.
func (f *exfatFile) ReadAt(p []byte, off int64) (int, error) {
	if f.closed.Load() {
		return 0, os.ErrClosed
	}
	if off < 0 {
		return 0, fmt.Errorf("exfat: ReadAt: negative offset %d", off)
	}
	if off >= f.size {
		return 0, io.EOF
	}
	n := 0
	for n < len(p) {
		cur := off + int64(n)
		if cur >= f.size {
			return n, io.EOF
		}
		idx := cur / f.clusterSize
		within := cur % f.clusterSize
		chunk := f.clusterSize - within
		if rem := f.size - cur; chunk > rem {
			chunk = rem
		}
		if want := int64(len(p) - n); chunk > want {
			chunk = want
		}
		diskOff := f.dataBase + int64(f.clusters[idx]-2)*f.clusterSize + within
		m, err := f.fs.f.ReadAt(p[n:n+int(chunk)], diskOff)
		n += m
		if err != nil {
			return n, fmt.Errorf("exfat: read cluster %d: %w", f.clusters[idx], err)
		}
	}
	return n, nil
}
