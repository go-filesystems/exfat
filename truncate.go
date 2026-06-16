package filesystem_exfat

import (
	"encoding/binary"
	"fmt"

	filesystem "github.com/go-filesystems/interface"
)

// Verify implementation of the optional Truncater interface.
var _ filesystem.Truncater = (*exfatFS)(nil)

// Truncate resizes the regular file at path to newSize bytes. Shrinking
// drops the trailing data and frees the clusters that are no longer
// needed; growing extends the file with zero-fill. Either way the
// file's Stream extension DataLength / ValidDataLength are updated and
// the File entry's LastModifiedTimestamp is refreshed. Directories and
// the root are rejected.
//
// The rewrite reuses the driver's existing allocation/free machinery
// (writeData / freeChain), so the file is re-emitted with a canonical
// FAT chain regardless of whether the source used NoFatChain.
func (fs *exfatFS) Truncate(path string, newSize int64) error {
	if newSize < 0 {
		return fmt.Errorf("exfat: truncate: negative size %d", newSize)
	}
	name, parentCluster, err := fs.getParentDir(path)
	if err != nil {
		return err
	}
	if name == "" {
		return fmt.Errorf("exfat: %q is not a regular file", path)
	}
	rootBuf, err := fs.readDirBuf(parentCluster)
	if err != nil {
		return err
	}
	entryOff, secondaryCount := exfatFindEntry(rootBuf, name)
	if entryOff < 0 {
		return fmt.Errorf("exfat: %q not found", path)
	}
	attrs := binary.LittleEndian.Uint16(rootBuf[entryOff+4 : entryOff+6])
	if attrs&uint16(exfatAttrDir) != 0 {
		return fmt.Errorf("exfat: %q is a directory", path)
	}

	stream := rootBuf[entryOff+dirEntrySize : entryOff+2*dirEntrySize]
	oldCluster := binary.LittleEndian.Uint32(stream[20:24])
	oldSize := binary.LittleEndian.Uint64(stream[24:32])

	if uint64(newSize) == oldSize {
		return nil
	}

	// Read the current contents (bounded by the old size), then build the
	// new body: the truncated prefix for shrink, or the existing bytes
	// followed by implicit zero-fill for grow.
	old, err := fs.readClusterChain(oldCluster, oldSize)
	if err != nil {
		return err
	}
	newData := make([]byte, newSize)
	copy(newData, old) // copies min(len(old), newSize) bytes; the rest stays zero

	// Free the old chain before allocating the new one so shrink reclaims
	// clusters and grow can reuse them.
	if oldCluster >= 2 {
		if err := fs.freeChain(oldCluster); err != nil {
			return err
		}
	}
	var firstCluster uint32
	if len(newData) > 0 {
		firstCluster, err = fs.writeData(newData)
		if err != nil {
			return err
		}
	}

	// Update the Stream extension: first cluster, ValidDataLength,
	// DataLength, and the AllocationPossible / NoFatChain flags. We emit a
	// FAT chain (NoFatChain clear) to match writeData; AllocationPossible
	// is set only while clusters are allocated.
	binary.LittleEndian.PutUint64(stream[8:16], uint64(newSize))  // ValidDataLength
	binary.LittleEndian.PutUint32(stream[20:24], firstCluster)    // FirstCluster
	binary.LittleEndian.PutUint64(stream[24:32], uint64(newSize)) // DataLength
	if firstCluster >= 2 {
		stream[1] = 1 // AllocationPossible, NoFatChain clear
	} else {
		stream[1] = 0
	}

	// Refresh the modify timestamp on the File entry.
	binary.LittleEndian.PutUint32(rootBuf[entryOff+12:entryOff+16], exfatNowTimestamp())

	// Recompute the entry-set checksum over the File entry and its
	// secondaries, then write the directory back.
	setLen := (secondaryCount + 1) * dirEntrySize
	binary.LittleEndian.PutUint16(rootBuf[entryOff+2:entryOff+4],
		exfatEntrySetChecksum(rootBuf[entryOff:entryOff+setLen]))
	return fs.writeDirBuf(parentCluster, rootBuf)
}
