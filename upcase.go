package filesystem_exfat

import (
	"encoding/binary"
	"strings"
	"unicode/utf16"
)

// The name hash of a directory entry is taken over the name up-cased with the
// volume's OWN Up-case Table (the 0x82 entry of the root directory), not with
// the host's Unicode rules. The Linux driver and fsck.exfat both read that
// table from the volume, so a hash made with Go's strings.ToUpper disagrees
// with them for any letter the table does not map the same way: with our
// minimal table (ASCII only) "é" is not up-cased, and a hash over "É" made the
// kernel unable to find the file (#36).

// loadUpcase returns the volume's table as a 65536-entry slice (identity for
// an absent or unreadable table), decoded as the exFAT compressed format: a
// value 0xFFFF followed by N says "the next N code units map to themselves".
func (fs *exfatFS) loadUpcase() []uint16 {
	if fs.upcase != nil {
		return fs.upcase
	}
	tbl := identityUpcase()
	fs.upcase = tbl
	root, err := fs.readDirBuf(fs.info.RootDirectoryCluster)
	if err != nil {
		return tbl
	}
	for off := 0; off+dirEntrySize <= len(root); off += dirEntrySize {
		if root[off] != 0x82 {
			continue
		}
		cluster := binary.LittleEndian.Uint32(root[off+20:])
		size := binary.LittleEndian.Uint64(root[off+24:])
		if cluster < 2 || size == 0 || size > 1<<17 {
			return tbl
		}
		raw, err := fs.readContiguousRun(cluster, size)
		if err != nil || len(raw) < 2 {
			return tbl
		}
		fs.upcase = decodeUpcase(raw)
		break
	}
	return fs.upcase
}

func identityUpcase() []uint16 {
	t := make([]uint16, 1<<16)
	for i := range t {
		t[i] = uint16(i)
	}
	return t
}

func decodeUpcase(raw []byte) []uint16 {
	t := identityUpcase()
	idx := 0
	for i := 0; i+2 <= len(raw) && idx < len(t); i += 2 {
		v := binary.LittleEndian.Uint16(raw[i:])
		if v == 0xFFFF && i+4 <= len(raw) {
			idx += int(binary.LittleEndian.Uint16(raw[i+2:]))
			i += 2
			continue
		}
		t[idx] = v
		idx++
	}
	return t
}

// entrySet builds a directory entry set whose NameHash follows this volume's table.
func (fs *exfatFS) entrySet(name string, attrs uint16, cluster uint32, size uint64) []byte {
	return makeExFATEntrySetHashed(name, attrs, cluster, size, fs.nameHash(name))
}

// nameHash is the entry-set NameHash of name under this volume's table.
func (fs *exfatFS) nameHash(name string) uint16 {
	return hashWithTable(name, fs.loadUpcase())
}

func hashWithTable(name string, tbl []uint16) uint16 {
	var hash uint16
	for _, w := range utf16.Encode([]rune(name)) {
		w = tbl[w]
		hash = (hash >> 1) | (hash << 15)
		hash += uint16(w & 0xFF)
		hash = (hash >> 1) | (hash << 15)
		hash += uint16(w >> 8)
	}
	return hash
}

// exfatNameHash is the hash under the Up-case Table Format writes (ASCII
// only), kept for callers that have no volume.
func exfatNameHash(name string) uint16 {
	return hashWithTable(asciiUpper(name), identityUpcase())
}

func asciiUpper(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' {
			return r - 32
		}
		return r
	}, s)
}
