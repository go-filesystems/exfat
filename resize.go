package filesystem_exfat

import (
	"encoding/binary"
	"fmt"
	"os"
)

// resizeFile is the subset of *os.File that Grow/Shrink need. Pulled into
// an interface so tests can wire a wrapper that, for example, validates
// truncate semantics without touching the host filesystem.
type resizeFile interface {
	Truncate(int64) error
}

// Grow extends the volume so that the backing image spans newSizeBytes.
// The new size must be strictly larger than the current VolumeLength and
// be cluster-aligned. The Allocation Bitmap must still fit inside the
// cluster(s) it already occupies, and the FAT must still cover every
// addressable cluster — Grow refuses otherwise rather than relocating
// system files. The boot region and its backup are rewritten with the
// new VolumeLength / ClusterCount and a fresh Boot Checksum.
func (fs *exfatFS) Grow(newSizeBytes int64) error {
	return fs.resize(newSizeBytes, false)
}

// Shrink truncates the volume so that the backing image spans
// newSizeBytes. The call is refused when any cluster beyond the new
// end-of-volume is currently allocated (per the Allocation Bitmap),
// when newSizeBytes is not strictly smaller than the current
// VolumeLength, or when it isn't cluster-aligned. On success the boot
// region, backup, bitmap header, and backing file are all updated.
func (fs *exfatFS) Shrink(newSizeBytes int64) error {
	return fs.resize(newSizeBytes, true)
}

// Resize chooses between Grow and Shrink based on the requested size.
// Passing exactly the current VolumeLength is a no-op (returns nil).
func (fs *exfatFS) Resize(newSizeBytes int64) error {
	curBytes := int64(fs.info.VolumeLength) * int64(fs.info.BytesPerSector())
	switch {
	case newSizeBytes == curBytes:
		return nil
	case newSizeBytes > curBytes:
		return fs.Grow(newSizeBytes)
	default:
		return fs.Shrink(newSizeBytes)
	}
}

// resize is the shared driver of Grow/Shrink. shrink=true selects the
// shrink path (capacity-decreasing); shrink=false selects the grow path.
func (fs *exfatFS) resize(newSizeBytes int64, shrink bool) error {
	if newSizeBytes <= 0 {
		return fmt.Errorf("exfat: resize: invalid size %d", newSizeBytes)
	}
	bytesPerSector := int64(fs.info.BytesPerSector())
	clusterSize := int64(fs.info.ClusterSize())

	if newSizeBytes%clusterSize != 0 {
		return fmt.Errorf("exfat: resize: size %d is not a multiple of cluster size %d",
			newSizeBytes, clusterSize)
	}

	curBytes := int64(fs.info.VolumeLength) * bytesPerSector
	if shrink {
		if newSizeBytes >= curBytes {
			return fmt.Errorf("exfat: shrink: new size %d must be smaller than current %d",
				newSizeBytes, curBytes)
		}
	} else {
		if newSizeBytes <= curBytes {
			return fmt.Errorf("exfat: grow: new size %d must be larger than current %d",
				newSizeBytes, curBytes)
		}
	}

	newTotalSectors := uint64(newSizeBytes) / uint64(bytesPerSector)
	if newTotalSectors <= uint64(fs.info.ClusterHeapOffset) {
		return fmt.Errorf("exfat: resize: size %d does not leave room for the cluster heap", newSizeBytes)
	}
	newClusterCount := uint32((newTotalSectors - uint64(fs.info.ClusterHeapOffset)) /
		uint64(fs.info.SectorsPerCluster()))
	if newClusterCount < 3 {
		return fmt.Errorf("exfat: resize: size %d leaves only %d data clusters; need ≥3", newSizeBytes, newClusterCount)
	}
	// Root directory cluster must still be valid after the resize.
	if fs.info.RootDirectoryCluster > newClusterCount+1 {
		return fmt.Errorf("exfat: shrink: root directory cluster %d is beyond new cluster count %d",
			fs.info.RootDirectoryCluster, newClusterCount)
	}

	// The FAT covers (clusterCount + 2) 32-bit entries. The on-disk FAT
	// length is fixed; refuse to grow past what the existing FAT can
	// address rather than silently relocating cluster heap content.
	fatCapacityEntries := (uint64(fs.info.FATLength) * uint64(bytesPerSector)) / 4
	if uint64(newClusterCount)+2 > fatCapacityEntries {
		return fmt.Errorf("exfat: resize: new cluster count %d exceeds FAT capacity %d",
			newClusterCount, fatCapacityEntries-2)
	}

	// Allocation Bitmap capacity check. The bitmap stores 1 bit per
	// cluster. Its on-disk allocation (chain starting at bitmapCluster)
	// must hold ⌈newClusterCount/8⌉ bytes. We refuse to relocate or grow
	// the bitmap chain itself; if the new size doesn't fit, the caller
	// must reformat at the larger size.
	newBitmapBytes := (uint64(newClusterCount) + 7) / 8
	if fs.bitmapCluster >= 2 {
		bitmapCapacity, err := fs.bitmapChainCapacity()
		if err != nil {
			return err
		}
		if newBitmapBytes > bitmapCapacity {
			return fmt.Errorf("exfat: resize: new bitmap needs %d bytes; current allocation holds %d",
				newBitmapBytes, bitmapCapacity)
		}
	}

	if shrink {
		// Refuse the shrink if any cluster ≥ (newClusterCount+2) is
		// currently allocated. Walk the bitmap when available; fall
		// back to the FAT entries otherwise.
		if err := fs.assertNoAllocBeyond(newClusterCount); err != nil {
			return err
		}
	}

	// ── Phase 1: extend backing file (grow only) ──────────────────────────
	// We extend before rewriting metadata so the new tail bytes exist
	// (and are zero) when we update the bitmap header. The new bytes
	// inside any extended bitmap region are already zero (= free).
	if !shrink {
		if rf, ok := fs.f.(resizeFile); ok {
			if err := rf.Truncate(newSizeBytes); err != nil {
				return fmt.Errorf("exfat: grow: truncate to %d: %w", newSizeBytes, err)
			}
		} else {
			return fmt.Errorf("exfat: resize: backing file does not support Truncate")
		}
	}

	// ── Phase 2: rewrite Main + Backup boot regions ──────────────────────
	if err := fs.rewriteBootRegion(newTotalSectors, newClusterCount); err != nil {
		return err
	}

	// ── Phase 3: update Allocation Bitmap directory entry ────────────────
	if err := fs.updateBitmapHeader(newBitmapBytes); err != nil {
		return err
	}

	// ── Phase 4: shrink backing file last ────────────────────────────────
	if shrink {
		if rf, ok := fs.f.(resizeFile); ok {
			if err := rf.Truncate(newSizeBytes); err != nil {
				return fmt.Errorf("exfat: shrink: truncate to %d: %w", newSizeBytes, err)
			}
		} else {
			return fmt.Errorf("exfat: resize: backing file does not support Truncate")
		}
	}

	// ── Phase 5: refresh cached Info ─────────────────────────────────────
	fs.info.VolumeLength = newTotalSectors
	fs.info.ClusterCount = newClusterCount
	fs.bitmapLength = newBitmapBytes
	return nil
}

// bitmapChainCapacity returns the byte length of the cluster chain
// backing the Allocation Bitmap. It walks the FAT starting at
// bitmapCluster until it hits an EOC marker or the chain stops being a
// valid cluster reference. The result is "physical bytes that could
// store bitmap data", which is the upper bound DataLength can grow to
// without relocating the chain.
func (fs *exfatFS) bitmapChainCapacity() (uint64, error) {
	if fs.bitmapCluster < 2 {
		return 0, nil
	}
	clusterSize := uint64(fs.info.ClusterSize())
	fatBase := fs.info.FATOffsetBytes(fs.partOffset)
	cluster := fs.bitmapCluster
	var clusters uint64
	// Bound the walk defensively so a corrupted FAT can't spin forever.
	for i := 0; i < int(fs.info.ClusterCount)+2; i++ {
		clusters++
		var next [4]byte
		if _, err := fs.f.ReadAt(next[:], fatBase+int64(cluster)*4); err != nil {
			return 0, fmt.Errorf("exfat: read FAT entry for bitmap cluster %d: %w", cluster, err)
		}
		nextCluster := binary.LittleEndian.Uint32(next[:])
		if nextCluster >= 0xFFFFFFF8 {
			break
		}
		if nextCluster < 2 {
			break
		}
		cluster = nextCluster
	}
	return clusters * clusterSize, nil
}

// assertNoAllocBeyond returns a descriptive error when any cluster
// index ≥ (newClusterCount+2) is currently in use. Prefers the
// Allocation Bitmap (fast, O(N/8)) and falls back to a FAT scan when
// the volume has no bitmap entry (older minimal images).
func (fs *exfatFS) assertNoAllocBeyond(newClusterCount uint32) error {
	if fs.bitmapCluster >= 2 && fs.bitmapLength > 0 {
		return fs.assertNoAllocBeyondViaBitmap(newClusterCount)
	}
	return fs.assertNoAllocBeyondViaFAT(newClusterCount)
}

func (fs *exfatFS) assertNoAllocBeyondViaBitmap(newClusterCount uint32) error {
	// The byte that holds the bit for the *first* cluster beyond the
	// new heap is byte index (newClusterCount / 8) — because bit i is
	// for cluster (i+2). bit (newClusterCount) is the first one that
	// must be zero for the shrink to be legal.
	firstForbiddenBit := uint64(newClusterCount)
	totalBits := uint64(fs.info.ClusterCount)
	if firstForbiddenBit >= totalBits {
		return nil
	}
	startByte := firstForbiddenBit / 8
	endByte := fs.bitmapLength
	if endByte > (totalBits+7)/8 {
		endByte = (totalBits + 7) / 8
	}
	if startByte >= endByte {
		return nil
	}
	// Walk the bitmap chain into one contiguous buffer. We read the
	// whole chain at once: even with the spec's 2^32-cluster ceiling
	// the bitmap is bounded at ⌈ClusterCount/8⌉ bytes, and the in-fmt
	// default case (single cluster) keeps this trivial.
	buf, err := fs.readBitmapBytes(startByte, endByte-startByte)
	if err != nil {
		return err
	}
	// Mask off the in-range bits in the first byte we touched.
	leadingBitsToIgnore := uint8(firstForbiddenBit % 8)
	if leadingBitsToIgnore > 0 {
		buf[0] &^= (1 << leadingBitsToIgnore) - 1
	}
	for i, b := range buf {
		if b == 0 {
			continue
		}
		// Find offending bit for the diagnostic.
		for bit := 0; bit < 8; bit++ {
			if b&(1<<bit) != 0 {
				clusterIdx := 2 + (startByte+uint64(i))*8 + uint64(bit)
				return fmt.Errorf("exfat: shrink: cluster %d is allocated beyond new cluster count %d",
					clusterIdx, newClusterCount)
			}
		}
	}
	return nil
}

// readBitmapBytes returns the slice of bitmap bytes [start, start+n)
// from the Allocation Bitmap chain. The chain is walked one cluster at
// a time; reads are issued cluster-aligned within each cluster so
// nothing weird happens on the boundaries. Out-of-range reads (past
// the chain) return the bytes that were available so far without an
// error — the caller has already vetted the requested range against
// fs.bitmapLength.
func (fs *exfatFS) readBitmapBytes(start, n uint64) ([]byte, error) {
	out := make([]byte, n)
	if n == 0 {
		return out, nil
	}
	clusterSize := uint64(fs.info.ClusterSize())
	dataBase := fs.info.ClusterHeapOffsetBytes(fs.partOffset)
	fatBase := fs.info.FATOffsetBytes(fs.partOffset)
	cluster := fs.bitmapCluster
	// Skip whole clusters to reach the starting byte.
	for start >= clusterSize {
		var next [4]byte
		if _, err := fs.f.ReadAt(next[:], fatBase+int64(cluster)*4); err != nil {
			return nil, fmt.Errorf("exfat: read FAT entry for bitmap cluster %d: %w", cluster, err)
		}
		nc := binary.LittleEndian.Uint32(next[:])
		if nc < 2 || nc >= 0xFFFFFFF8 {
			return out, nil
		}
		cluster = nc
		start -= clusterSize
	}
	written := uint64(0)
	innerOffset := start
	for written < n {
		off := dataBase + int64(cluster-2)*int64(clusterSize) + int64(innerOffset)
		toRead := clusterSize - innerOffset
		if toRead > n-written {
			toRead = n - written
		}
		if _, err := fs.f.ReadAt(out[written:written+toRead], off); err != nil {
			return nil, fmt.Errorf("exfat: read bitmap segment: %w", err)
		}
		written += toRead
		if written >= n {
			break
		}
		var next [4]byte
		if _, err := fs.f.ReadAt(next[:], fatBase+int64(cluster)*4); err != nil {
			return nil, fmt.Errorf("exfat: read FAT entry for bitmap cluster %d: %w", cluster, err)
		}
		nc := binary.LittleEndian.Uint32(next[:])
		if nc < 2 || nc >= 0xFFFFFFF8 {
			break
		}
		cluster = nc
		innerOffset = 0
	}
	return out, nil
}

func (fs *exfatFS) assertNoAllocBeyondViaFAT(newClusterCount uint32) error {
	fatBase := fs.info.FATOffsetBytes(fs.partOffset)
	var buf [4]byte
	for c := uint32(newClusterCount) + 2; c < fs.info.ClusterCount+2; c++ {
		if _, err := fs.f.ReadAt(buf[:], fatBase+int64(c)*4); err != nil {
			return fmt.Errorf("exfat: read FAT entry: %w", err)
		}
		if binary.LittleEndian.Uint32(buf[:]) != 0 {
			return fmt.Errorf("exfat: shrink: cluster %d is allocated beyond new cluster count %d",
				c, newClusterCount)
		}
	}
	return nil
}

// rewriteBootRegion rewrites the Main Boot Sector (sector 0) and the
// Backup Boot Sector (sector 12) with the new VolumeLength /
// ClusterCount, then recomputes the Boot Checksum and writes its sector
// (11 main, 23 backup). All other sectors of the boot regions
// (extended boot, OEM parameters, reserved) are left untouched — they
// already round-trip through Format and don't depend on the volume size.
func (fs *exfatFS) rewriteBootRegion(newTotalSectors uint64, newClusterCount uint32) error {
	bytesPerSector := int64(fs.info.BytesPerSector())
	bootRegionBytes := int64(11) * bytesPerSector

	// Main boot region read (sectors 0..10).
	mainRegion := make([]byte, bootRegionBytes)
	if _, err := fs.f.ReadAt(mainRegion, fs.partOffset); err != nil {
		return fmt.Errorf("exfat: resize: read main boot region: %w", err)
	}
	patchBootSector(mainRegion[:bytesPerSector], newTotalSectors, newClusterCount)
	if _, err := fs.f.WriteAt(mainRegion[:bytesPerSector], fs.partOffset); err != nil {
		return fmt.Errorf("exfat: resize: write main boot sector: %w", err)
	}
	mainChecksum := exfatBootChecksum(mainRegion)
	checksumSector := make([]byte, bytesPerSector)
	for off := int64(0); off < bytesPerSector; off += 4 {
		binary.LittleEndian.PutUint32(checksumSector[off:], mainChecksum)
	}
	if _, err := fs.f.WriteAt(checksumSector, fs.partOffset+11*bytesPerSector); err != nil {
		return fmt.Errorf("exfat: resize: write main boot checksum: %w", err)
	}

	// Backup boot region: per the spec the backup is a verbatim copy of
	// the main. Rewrite both sector 12 (boot sector) and sector 23
	// (checksum) — the in-between sectors (13..22) carry data that
	// should already match the main region, but we don't touch them.
	backupBoot := make([]byte, bytesPerSector)
	copy(backupBoot, mainRegion[:bytesPerSector])
	if _, err := fs.f.WriteAt(backupBoot, fs.partOffset+12*bytesPerSector); err != nil {
		return fmt.Errorf("exfat: resize: write backup boot sector: %w", err)
	}
	if _, err := fs.f.WriteAt(checksumSector, fs.partOffset+23*bytesPerSector); err != nil {
		return fmt.Errorf("exfat: resize: write backup boot checksum: %w", err)
	}
	return nil
}

// patchBootSector mutates the in-memory main boot sector with the new
// VolumeLength (bytes 72..79) and ClusterCount (bytes 92..95). All
// other fields — including the BPB shifts that determine sector and
// cluster geometry — are intentionally left untouched.
func patchBootSector(sector []byte, newTotalSectors uint64, newClusterCount uint32) {
	le := binary.LittleEndian
	le.PutUint64(sector[72:80], newTotalSectors)
	le.PutUint32(sector[92:96], newClusterCount)
}

// updateBitmapHeader rewrites the DataLength field of the Allocation
// Bitmap entry in the root directory cluster to newBitmapBytes. When
// the volume has no bitmap entry (legal for older minimal images), the
// call is a no-op.
func (fs *exfatFS) updateBitmapHeader(newBitmapBytes uint64) error {
	if fs.bitmapCluster < 2 {
		return nil
	}
	rootBuf, err := fs.readDirBuf(fs.info.RootDirectoryCluster)
	if err != nil {
		return fmt.Errorf("exfat: resize: read root directory: %w", err)
	}
	for offset := 0; offset+dirEntrySize <= len(rootBuf); offset += dirEntrySize {
		typ := rootBuf[offset]
		if typ == exfatEntryEnd {
			break
		}
		if typ != 0x81 {
			continue
		}
		binary.LittleEndian.PutUint64(rootBuf[offset+24:offset+32], newBitmapBytes)
		return fs.writeDirBuf(fs.info.RootDirectoryCluster, rootBuf)
	}
	return nil
}

// Ensure *os.File satisfies resizeFile at compile time (defensive check
// so any future refactor that changes the underlying diskRW concrete
// type still has Truncate available).
var _ resizeFile = (*os.File)(nil)
