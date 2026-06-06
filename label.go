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

// SetLabel writes a new Volume Label entry at offset 0 of the root
// directory cluster. The label is capped at MaxLabelLen UTF-16 code
// units. An empty label writes a "not-in-use" marker (entry type
// 0x03) — kernel and Windows treat both as "no label".
//
// The slot at root entry 0 must currently be either:
//   - an existing Volume Label entry (0x83 or 0x03) — replaced in place, or
//   - the End-of-Directory marker (0x00) — replaced and EOD is preserved
//     at offset 32 since the cluster was zeroed.
//
// A non-label, non-EOD entry at offset 0 is rejected: the round assumes
// the label always lives at the start of the root cluster (the layout
// produced by Format and by mkfs.exfat).
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

	// Read current root entry 0.
	rootOff := fs.info.RootDirOffset(fs.partOffset)
	current := make([]byte, dirEntrySize)
	if _, err := fs.f.ReadAt(current, rootOff); err != nil {
		return fmt.Errorf("exfat SetLabel: read root entry 0: %w", err)
	}
	switch current[0] {
	case exfatEntryVolumeLabel, exfatEntryVolumeLabelUnused, exfatEntryEnd:
		// OK — safe to replace.
	default:
		return fmt.Errorf("exfat SetLabel: root entry 0 has type 0x%02x; label slot must be 0x83 / 0x03 / 0x00", current[0])
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

	if _, err := fs.f.WriteAt(entry, rootOff); err != nil {
		return fmt.Errorf("exfat SetLabel: write root entry 0: %w", err)
	}
	fs.label = label
	return nil
}

// readVolumeLabel inspects the first directory entry of the root cluster
// and returns the decoded label string. Returns "" if entry 0 is not a
// Volume Label entry. Called from Open so the cached fs.label is fresh.
func readVolumeLabel(rd diskRW, info Info, partOffset int64) (string, error) {
	off := info.RootDirOffset(partOffset)
	buf := make([]byte, dirEntrySize)
	if _, err := rd.ReadAt(buf, off); err != nil {
		return "", fmt.Errorf("exfat: read root entry 0: %w", err)
	}
	if buf[0] != exfatEntryVolumeLabel {
		return "", nil // not-in-use marker or non-label entry → empty label
	}
	count := int(buf[1])
	if count < 0 || count > MaxLabelLen {
		return "", fmt.Errorf("exfat: Volume Label entry has invalid count %d", count)
	}
	words := make([]uint16, count)
	for i := 0; i < count; i++ {
		words[i] = binary.LittleEndian.Uint16(buf[2+i*2:])
	}
	return string(utf16.Decode(words)), nil
}
