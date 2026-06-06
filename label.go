package filesystem_exfat

import (
	"encoding/binary"
	"fmt"
	"unicode/utf16"

	filesystem "github.com/go-filesystems/interface"
)

// MaxLabelLen is the maximum number of UTF-16 code units storable in
// the exFAT Volume Label entry (per the exFAT specification §7.3).
const MaxLabelLen = 11

// Volume Label directory entry types (high bit = in-use flag).
const (
	exfatEntryVolumeLabel       = 0x83 // in-use
	exfatEntryVolumeLabelUnused = 0x03 // deleted / not-in-use
)

// Compile-time assertion: exfatFS implements filesystem.Labeller.
var _ filesystem.Labeller = (*exfatFS)(nil)

// Label returns the current volume label, decoded from the first
// directory entry in the root cluster (if it is a Volume Label entry).
func (fs *exfatFS) Label() string {
	return fs.label
}

// findLabelSlot scans the root directory cluster looking for an existing
// Volume Label entry (0x83 or 0x03). If one is found, its byte offset
// within the cluster is returned. Otherwise findLabelSlot returns the
// offset of the first End-of-Directory marker (0x00) — a free slot the
// caller can overwrite. Returns -1 when the root cluster is full.
//
// This scan tolerates the system-file entries (0x81 Bitmap, 0x82 Upcase)
// that Format emits ahead of any user content: the label entry can live
// anywhere in the root, not just at offset 0.
func findLabelSlot(rootBuf []byte) int {
	for offset := 0; offset+dirEntrySize <= len(rootBuf); offset += dirEntrySize {
		switch rootBuf[offset] {
		case exfatEntryVolumeLabel, exfatEntryVolumeLabelUnused:
			return offset
		case exfatEntryEnd:
			return offset
		}
	}
	return -1
}

// SetLabel writes (or replaces) the Volume Label entry in the root
// directory. The label is capped at MaxLabelLen UTF-16 code units. An
// empty label writes a "not-in-use" marker (entry type 0x03) — kernel
// and Windows treat both as "no label".
//
// If a Volume Label entry already exists in the root cluster, it is
// replaced in place. Otherwise the entry is appended at the first
// End-of-Directory slot.
func (fs *exfatFS) SetLabel(label string) error {
	runes := []rune(label)
	wordCount := 0
	for _, r := range runes {
		// Each rune encodes to 1 (BMP) or 2 (surrogate pair) UTF-16 code units.
		if r < 0x10000 {
			wordCount++
		} else {
			wordCount += 2
		}
	}
	if wordCount > MaxLabelLen {
		return fmt.Errorf("exfat: label %q is %d UTF-16 code units, exceeds maximum %d", label, wordCount, MaxLabelLen)
	}

	rootBuf, err := fs.readDirBuf(fs.info.RootDirectoryCluster)
	if err != nil {
		return fmt.Errorf("exfat SetLabel: read root directory: %w", err)
	}
	slot := findLabelSlot(rootBuf)
	if slot < 0 {
		return fmt.Errorf("exfat SetLabel: root directory has no slot for a volume label")
	}

	// Build the new 32-byte Volume Label entry.
	entry := make([]byte, dirEntrySize)
	if wordCount == 0 {
		entry[0] = exfatEntryVolumeLabelUnused
	} else {
		entry[0] = exfatEntryVolumeLabel
	}
	entry[1] = uint8(wordCount)
	words := utf16.Encode(runes)
	for i, w := range words {
		binary.LittleEndian.PutUint16(entry[2+i*2:], w)
	}
	// bytes 2 + 2*wordCount .. 24 remain zero (pad)
	// bytes 24 .. 32 remain zero (reserved)

	copy(rootBuf[slot:slot+dirEntrySize], entry)
	if err := fs.writeDirBuf(fs.info.RootDirectoryCluster, rootBuf); err != nil {
		return fmt.Errorf("exfat SetLabel: write root directory: %w", err)
	}
	fs.label = label
	return nil
}

// readVolumeLabel scans the root directory cluster looking for a Volume
// Label entry (0x83) and returns the decoded label string. Returns ""
// (no error) when the root contains no in-use label entry. Called from
// Open so the cached fs.label is fresh.
//
// The scan walks the first cluster only — that is sufficient for both
// the layout produced by Format() and the layouts produced by mkfs.exfat
// / newfs_exfat, all of which place the label in the first root cluster.
func readVolumeLabel(rd diskRW, info Info, partOffset int64) (string, error) {
	clusterSize := info.ClusterSize()
	off := info.RootDirOffset(partOffset)
	buf := make([]byte, clusterSize)
	if _, err := rd.ReadAt(buf, off); err != nil {
		return "", fmt.Errorf("exfat: read root directory: %w", err)
	}
	for offset := 0; offset+dirEntrySize <= len(buf); offset += dirEntrySize {
		switch buf[offset] {
		case exfatEntryEnd:
			return "", nil
		case exfatEntryVolumeLabel:
			count := int(buf[offset+1])
			if count < 0 || count > MaxLabelLen {
				return "", fmt.Errorf("exfat: Volume Label entry has invalid count %d", count)
			}
			words := make([]uint16, count)
			for i := 0; i < count; i++ {
				words[i] = binary.LittleEndian.Uint16(buf[offset+2+i*2:])
			}
			return string(utf16.Decode(words)), nil
		}
	}
	return "", nil
}
