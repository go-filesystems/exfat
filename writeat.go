package filesystem_exfat

import (
	"encoding/binary"
	"fmt"
	"os"

	filesystem "github.com/go-filesystems/interface"
)

// Verify implementation of the optional write-at-an-offset interface.
//
// The assertion is on the File, not on the filesystem: writability is a
// property of the opened object, and this is what a caller's
// `f.(filesystem.WritableFile)` probe finds.
var _ filesystem.WritableFile = (*exfatFile)(nil)

// clustersPerScan is how many clusters allocClusterRun examines per pass:
// 1024, i.e. 4 KiB of FAT (one page) and 128 bytes of Allocation Bitmap.
//
// The number is also why the bitmap slice lines up: the scan starts at cluster
// 2 and steps by clustersPerScan, so the run's first bit is always byte
// (c-2)/8 of the bitmap with no partial-byte arithmetic.
const clustersPerScan = 1024

// exfatEndOfChain is the FAT value that terminates a chain. exFAT reserves
// 0xFFFFFFF8..0xFFFFFFFF; the driver writes 0xFFFFFFFF everywhere else, so
// this does too.
const exfatEndOfChain = 0xFFFFFFFF

// WriteAt writes len(p) bytes at off, in place.
//
// This is the method the whole type exists for. Before it, a positional write
// on exFAT could only be expressed as ReadFile + splice + WriteFile: the whole
// file read, the whole file reallocated, the whole file written back, for
// every request. Here the allocation resolved at OpenFile turns an offset into
// (cluster index, offset within cluster) by division, and the cost is the
// bytes the caller actually asked for, plus one allocation scan when the file
// has to grow.
//
// It follows io.WriterAt to the letter: it writes all of p or returns a
// non-nil error, and it never reports a short write with a nil error, which a
// caller reads as success. It DOES extend the file — an offset past the
// current end is legal, and the gap between the old end and off reads back as
// zeros, exactly as ReadFile would report it, because the clusters covering
// the gap are zero-filled as they are allocated and the slack in the old last
// cluster is zeroed too.
//
// Size follows immediately: a WritableFile is a handle the caller mutates, not
// the snapshot a read-only File is, and the Stream Extension entry is
// rewritten before WriteAt returns so a Stat through the Filesystem agrees as
// well.
//
// Concurrency: the file's own lock is held exclusively, so concurrent WriteAt
// calls are serialised — stricter than io.WriterAt requires, and correct for
// overlapping ranges too. It says nothing about another handle on the same
// volume: as everywhere else in this driver, two Filesystem-level writers are
// the caller's problem, and this File's lock cannot see them.
func (f *exfatFile) WriteAt(p []byte, off int64) (int, error) {
	if f.closed.Load() {
		return 0, os.ErrClosed
	}
	if off < 0 {
		return 0, fmt.Errorf("exfat: WriteAt: negative offset %d", off)
	}
	if len(p) == 0 {
		return 0, nil
	}
	// off is caller-supplied and len(p) can be large; compute the end before
	// anything derives a cluster index from it, so an overflow becomes an
	// error rather than a negative offset far inside the volume. Unlike
	// FAT32, exFAT records the length in 64 bits, so there is no field-width
	// ceiling to hit — the real ceiling is the cluster heap, and refusing at
	// it here means a hopeless write fails before it has allocated anything.
	end := off + int64(len(p))
	if end < off || uint64(end) > f.fs.maxChainBytes() {
		return 0, fmt.Errorf("exfat: WriteAt: offset %d + %d bytes exceeds what the cluster heap can address (%d bytes)",
			off, len(p), f.fs.maxChainBytes())
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if end > f.size {
		if err := f.resizeLocked(end); err != nil {
			return 0, err
		}
	}

	n := 0
	for n < len(p) {
		cur := off + int64(n)
		idx := cur / f.clusterSize
		within := cur % f.clusterSize
		chunk := f.clusterSize - within
		if want := int64(len(p) - n); chunk > want {
			chunk = want
		}
		diskOff := f.dataBase + int64(f.clusters[idx]-2)*f.clusterSize + within
		m, err := f.fs.f.WriteAt(p[n:n+int(chunk)], diskOff)
		n += m
		if err != nil {
			return n, fmt.Errorf("exfat: write cluster %d: %w", f.clusters[idx], err)
		}
	}
	return n, nil
}

// Truncate resizes the file to size bytes.
//
// Growing extends it with zeros: exFAT has no sparse representation, so the
// new clusters are really allocated and really written as zeros, and a caller
// must not read a successful grow as "free". Shrinking frees the clusters past
// the new end — in the FAT and in the Allocation Bitmap both — and zeroes the
// slack in the last one kept, so a later grow reads zeros rather than the
// bytes that used to be there, which is the same rule the path-scoped Truncate
// on the Filesystem follows.
//
// The Stream Extension entry is rewritten before Truncate returns, so Size and
// a Filesystem-level Stat agree at once.
func (f *exfatFile) Truncate(size int64) error {
	if f.closed.Load() {
		return os.ErrClosed
	}
	if size < 0 {
		return fmt.Errorf("exfat: Truncate: negative size %d", size)
	}
	if uint64(size) > f.fs.maxChainBytes() {
		return fmt.Errorf("exfat: Truncate: size %d exceeds what the cluster heap can address (%d bytes)",
			size, f.fs.maxChainBytes())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if size == f.size {
		return nil
	}
	return f.resizeLocked(size)
}

// Sync reports whether everything written through this File has reached the
// backing store.
//
// What it can promise depends on what the volume was opened over, and saying
// so is the point of the method: this driver buffers NOTHING of its own —
// WriteAt has already issued the write to the image before it returns, and the
// directory entry with it — so Sync's whole job is to push the layer beneath.
// When that layer has a Sync (an *os.File, which is the normal case: Open uses
// os.OpenFile, and this is fsync(2)) it is called and its error returned. When
// it has none — an in-memory image in a test, a caller's own io.WriterAt — the
// data is already as durable as that layer makes it and Sync returns nil,
// having done nothing, because there is nothing left to do rather than because
// the guarantee was quietly dropped.
//
// A server answering NFSv3 COMMIT can therefore report FILE_SYNC honestly on a
// file-backed image, which is the case that matters.
func (f *exfatFile) Sync() error {
	if f.closed.Load() {
		return os.ErrClosed
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	if s, ok := f.fs.f.(interface{ Sync() error }); ok {
		if err := s.Sync(); err != nil {
			return fmt.Errorf("exfat: sync image: %w", err)
		}
	}
	return nil
}

// resizeLocked changes the file's length to newSize, allocating or freeing
// clusters and rewriting the Stream Extension entry. f.mu must be held for
// writing.
//
// It is the O(clusters added or freed) counterpart of the Filesystem's
// path-scoped Truncate, which reads the ENTIRE file, frees its whole
// allocation and writes the entire body back on every call. Here the
// allocation is already in memory, so the tail is f.clusters[len-1] and
// nothing has to be re-read — that difference is what turns a sequence of
// appending writes from quadratic into linear.
func (f *exfatFile) resizeLocked(newSize int64) error {
	// A length change needs a real chain. Do this first, while the file still
	// describes exactly the clusters it holds, so a failure here leaves a
	// perfectly consistent NoFatChain file rather than a half-converted one.
	if f.contiguous {
		if err := f.materialiseChainLocked(); err != nil {
			return err
		}
	}

	oldClusters := int64(len(f.clusters))
	newClusters := (newSize + f.clusterSize - 1) / f.clusterSize

	switch {
	case newClusters > oldClusters:
		if err := f.growLocked(newClusters); err != nil {
			return err
		}
	case newClusters < oldClusters:
		if err := f.shrinkLocked(newClusters); err != nil {
			return err
		}
	}

	// Zero the slack between the old end of file and the end of the cluster
	// holding it. Growing past it must read as zeros, not as whatever the
	// cluster held when it was last used for something else.
	if newSize > f.size && oldClusters > 0 {
		if slack := f.size % f.clusterSize; slack != 0 {
			if err := f.zeroSlackLocked(f.clusters[oldClusters-1], slack); err != nil {
				return err
			}
		}
	}
	// Shrinking leaves the tail of the last retained cluster readable through
	// the raw image; zero it for the same reason the path-scoped Truncate
	// does, so a later grow cannot resurrect it.
	if newSize < f.size && newClusters > 0 {
		if slack := newSize % f.clusterSize; slack != 0 {
			if err := f.zeroSlackLocked(f.clusters[newClusters-1], slack); err != nil {
				return err
			}
		}
	}

	var first uint32
	if len(f.clusters) > 0 {
		first = f.clusters[0]
	}
	if err := f.fs.setStreamExtent(f.path, first, newSize); err != nil {
		return err
	}
	f.size = newSize
	return nil
}

// zeroSlackLocked writes zeros over cluster from offset slack to its end.
// f.mu must be held for writing.
func (f *exfatFile) zeroSlackLocked(cluster uint32, slack int64) error {
	zeros := make([]byte, f.clusterSize-slack)
	off := f.dataBase + int64(cluster-2)*f.clusterSize + slack
	if _, err := f.fs.f.WriteAt(zeros, off); err != nil {
		return fmt.Errorf("exfat: zero slack in cluster %d: %w", cluster, err)
	}
	return nil
}

// materialiseChainLocked turns a NoFatChain allocation into a real FAT chain
// and clears the flag. f.mu must be held for writing.
//
// A NoFatChain file says "my clusters are consecutive and the FAT holds
// nothing for me". That is a fine way to read a file and a bad way to grow
// one: the cluster after the run is very often already taken, and a file
// cannot stay contiguous across a hole. Rather than gamble on the next cluster
// being free — an optimisation whose fallback is this code anyway — every
// change of length converts the file to the canonical shape the rest of this
// driver emits, which is exactly what the path-scoped Truncate and WriteFile
// already do when they rewrite a file.
//
// Order matters: the FAT links are written FIRST and the flag cleared last, so
// a failure partway leaves an entry that still says NoFatChain and still reads
// correctly — the FAT entries written are simply ignored. The reverse order
// would leave a file claiming a chain that does not exist yet, which reads as
// one cluster and loses the rest.
func (f *exfatFile) materialiseChainLocked() error {
	for i := 0; i < len(f.clusters)-1; i++ {
		if err := f.fs.setFATEntry(f.clusters[i], f.clusters[i+1]); err != nil {
			return err
		}
	}
	if n := len(f.clusters); n > 0 {
		if err := f.fs.setFATEntry(f.clusters[n-1], exfatEndOfChain); err != nil {
			return err
		}
	}
	var first uint32
	if len(f.clusters) > 0 {
		first = f.clusters[0]
	}
	if err := f.fs.setStreamExtent(f.path, first, f.size); err != nil {
		return err
	}
	f.contiguous = false
	return nil
}

// growLocked extends the allocation to newClusters clusters. f.mu must be held
// for writing.
//
// The whole run is allocated in one scan, claimed, zero-filled and linked; on
// any failure every cluster taken is put back in BOTH the FAT and the
// Allocation Bitmap, so a failed grow leaves the volume's free-space
// accounting as it found it rather than leaking clusters no file references.
func (f *exfatFile) growLocked(newClusters int64) error {
	need := int(newClusters - int64(len(f.clusters)))
	run, err := f.fs.allocClusterRun(need)
	if err != nil {
		return err
	}
	rollback := func() {
		for _, c := range run {
			_ = f.fs.setFATEntry(c, 0)
			_ = f.fs.setBitmapBit(c, false)
		}
	}
	// Claim each cluster before writing to it, in the bitmap as well as the
	// FAT: on exFAT the bitmap is the authority on what is free, and an
	// unclaimed cluster would be handed out again by the next allocation.
	for _, c := range run {
		if err := f.fs.setBitmapBit(c, true); err != nil {
			rollback()
			return err
		}
		if err := f.fs.setFATEntry(c, exfatEndOfChain); err != nil {
			rollback()
			return err
		}
	}
	zero := make([]byte, f.clusterSize)
	for _, c := range run {
		if _, err := f.fs.f.WriteAt(zero, f.dataBase+int64(c-2)*f.clusterSize); err != nil {
			rollback()
			return fmt.Errorf("exfat: zero new cluster %d: %w", c, err)
		}
	}
	// Link the run internally, then onto the existing tail. Doing it in this
	// order means the file's chain is never briefly longer than its recorded
	// size: the run is unreachable from the file until the last link lands.
	for i := 0; i < len(run)-1; i++ {
		if err := f.fs.setFATEntry(run[i], run[i+1]); err != nil {
			rollback()
			return err
		}
	}
	if n := len(f.clusters); n > 0 {
		if err := f.fs.setFATEntry(f.clusters[n-1], run[0]); err != nil {
			rollback()
			return err
		}
	}
	f.clusters = append(f.clusters, run...)
	return nil
}

// shrinkLocked cuts the allocation down to newClusters clusters, freeing the
// rest in the FAT and in the Allocation Bitmap. f.mu must be held for writing.
// newClusters == 0 frees everything and leaves the file with no first cluster,
// which is how exFAT spells "empty".
func (f *exfatFile) shrinkLocked(newClusters int64) error {
	drop := f.clusters[newClusters:]
	if newClusters > 0 {
		// Terminate the retained chain FIRST. If freeing then fails partway,
		// the file still describes exactly the clusters it claims; the worst
		// outcome is clusters marked in use that nothing references, which
		// fsck reports and repairs. The reverse order would leave the file
		// pointing at clusters marked free — the failure that loses data.
		if err := f.fs.setFATEntry(f.clusters[newClusters-1], exfatEndOfChain); err != nil {
			return err
		}
	}
	for _, c := range drop {
		if err := f.fs.setFATEntry(c, 0); err != nil {
			return err
		}
		if err := f.fs.setBitmapBit(c, false); err != nil {
			return err
		}
	}
	f.clusters = f.clusters[:newClusters]
	return nil
}

// allocClusterRun returns n free cluster numbers, scanning the volume once.
//
// It is the batching counterpart of the old allocCluster, which read one
// 4-byte FAT entry per syscall and restarted from cluster 2 on every call:
// allocating k clusters that way cost O(k · clusterCount) reads, which for a
// file written in blocks is quadratic in the file's size on its own. This
// reads a page of FAT and the matching 128 bytes of Allocation Bitmap per
// pass, and collects the whole run in one sweep.
//
// A cluster is free only when BOTH say so. That is not belt-and-braces, it is
// the exFAT rule: the bitmap is the authority, and a NoFatChain file's FAT
// entries are zero — indistinguishable from free space if the FAT is all one
// looks at. The FAT is still consulted because this driver's own writers set
// it, and because an image may have no bitmap at all (Open tolerates that),
// in which case the FAT is all there is. Requiring both can only ever refuse a
// cluster, never hand out a used one.
//
// The scan is bounded by BOTH the volume's cluster count and the FAT's own
// declared length, so a boot sector claiming more clusters than its FAT can
// describe cannot make the scan read past the first FAT into its mirror.
// The clusters are returned unclaimed; the caller marks them.
func (fs *exfatFS) allocClusterRun(n int) ([]uint32, error) {
	if n <= 0 {
		return nil, nil
	}
	last := int64(fs.info.ClusterCount) + 2
	if fatEntries := int64(fs.info.FATLength) * int64(fs.info.BytesPerSector()) / 4; fatEntries < last {
		last = fatEntries
	}
	fatBase := fs.info.FATOffsetBytes(fs.partOffset)
	out := make([]uint32, 0, n)
	buf := make([]byte, clustersPerScan*4)
	for c := int64(2); c < last && len(out) < n; {
		want := int64(clustersPerScan)
		if rem := last - c; rem < want {
			want = rem
		}
		b := buf[:want*4]
		if _, err := fs.f.ReadAt(b, fatBase+c*4); err != nil {
			return nil, fmt.Errorf("exfat: read FAT entries from cluster %d: %w", c, err)
		}
		bits, err := fs.bitmapBitsFrom(uint32(c), want)
		if err != nil {
			return nil, err
		}
		for i := int64(0); i < want && len(out) < n; i++ {
			if binary.LittleEndian.Uint32(b[i*4:]) != 0 {
				continue
			}
			if int(i/8) < len(bits) && bits[i/8]&(1<<(i%8)) != 0 {
				continue
			}
			out = append(out, uint32(c+i))
		}
		c += want
	}
	if len(out) < n {
		return nil, fmt.Errorf("exfat: no free clusters")
	}
	return out, nil
}

// bitmapBitsFrom returns the Allocation Bitmap bytes covering count clusters
// starting at cluster first, whose bit 0 is the first cluster asked for.
//
// It returns an empty slice — not an error — when the volume has no bitmap, or
// when the range falls past the bitmap's recorded length. Both mean "the
// bitmap has nothing to say about these clusters", and the caller then relies
// on the FAT alone, which is exactly what this driver did everywhere before
// the bitmap was consulted at all. A short slice is handled the same way, per
// cluster, by the caller's length check.
//
// first is always 2 + a multiple of clustersPerScan (a multiple of 8), so the
// requested bits start on a byte boundary and no shifting is needed.
func (fs *exfatFS) bitmapBitsFrom(first uint32, count int64) ([]byte, error) {
	if fs.bitmapCluster < 2 || first < 2 {
		return nil, nil
	}
	startByte := uint64(first-2) / 8
	if startByte >= fs.bitmapLength {
		return nil, nil
	}
	n := uint64(count+7) / 8
	if avail := fs.bitmapLength - startByte; n > avail {
		n = avail
	}
	bits, err := fs.readBitmapBytes(startByte, n)
	if err != nil {
		return nil, fmt.Errorf("exfat: read allocation bitmap from cluster %d: %w", first, err)
	}
	return bits, nil
}

// setStreamExtent rewrites the first-cluster and length fields of the Stream
// Extension entry for path, and refreshes the File entry's modify timestamp.
//
// exFAT records a file's length in its directory entry set and nowhere else,
// so a positional write that extends the file is not complete until this
// lands: a Stat, a ReadFile, or a reopen would all still report the old
// length. The entry-set checksum covers every entry in the set, so it has to
// be recomputed and rewritten too — an entry set whose checksum does not match
// is one a canonical fsck deletes.
//
// It also settles the GeneralSecondaryFlags byte at AllocationPossible with
// NoFatChain CLEAR, which is what every writer in this driver emits: by the
// time this is called the file has a real FAT chain, because resizeLocked
// materialises one before changing any length.
//
// It is the file-scoped twin of the tail of the path-scoped Truncate, which
// does the same field writes; the two are kept separate because Truncate
// already holds the directory buffer it needs for its own checks and
// re-reading it here would cost a second pass for nothing.
func (fs *exfatFS) setStreamExtent(path string, firstCluster uint32, size int64) error {
	name, parentCluster, err := fs.getParentDir(path)
	if err != nil {
		return err
	}
	if name == "" {
		return fmt.Errorf("exfat: %q is not a regular file", path)
	}
	buf, err := fs.readDirBuf(parentCluster)
	if err != nil {
		return err
	}
	entryOff, secondaryCount := exfatFindEntry(buf, name)
	if entryOff < 0 {
		return fmt.Errorf("exfat: %q not found", path)
	}
	stream := buf[entryOff+dirEntrySize : entryOff+2*dirEntrySize]
	le := binary.LittleEndian
	le.PutUint64(stream[8:16], uint64(size))  // ValidDataLength
	le.PutUint32(stream[20:24], firstCluster) // FirstCluster
	le.PutUint64(stream[24:32], uint64(size)) // DataLength
	if firstCluster >= 2 {
		stream[1] = exfatFlagAllocPossible
	} else {
		stream[1] = 0
	}
	// POSIX refreshes mtime on a write, and exFAT has the field. Nothing in
	// the fleet reads it yet — interface.Stat has no time accessor — but
	// writing it costs nothing and means the volume is honest when a real OS
	// mounts it, which is the whole point of matching an on-disk format.
	le.PutUint32(buf[entryOff+12:entryOff+16], exfatNowTimestamp())

	setLen := (secondaryCount + 1) * dirEntrySize
	le.PutUint16(buf[entryOff+2:entryOff+4], exfatEntrySetChecksum(buf[entryOff:entryOff+setLen]))
	return fs.writeDirBuf(parentCluster, buf)
}
