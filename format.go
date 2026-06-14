package filesystem_exfat

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"

	filesystem "github.com/go-filesystems/interface"
)

// FormatConfig holds optional parameters for Format.
// All fields are optional; sensible defaults are used when left at their zero value.
type FormatConfig struct {
	// Label is the volume label (stored as a volume-label directory entry;
	// not in the boot sector for exFAT). Trimmed to 11 characters.
	Label string
	// VolumeSerialNumber is the 32-bit serial number. A random value is generated when zero.
	VolumeSerialNumber uint32
}

// Layout parameters for a freshly formatted exFAT volume.
//
// Cluster heap layout produced by Format:
//
//	cluster 2 = Allocation Bitmap   (1 bit per cluster, fits comfortably in 1 cluster)
//	cluster 3 = Up-case Table       (8-byte compressed identity table)
//	cluster 4 = Root Directory      (volume label + bitmap + upcase + user entries)
//	cluster 5..= free data clusters
//
// The Microsoft exFAT specification requires both an Allocation Bitmap and
// an Up-case Table system file (referenced from the root directory via
// 0x81 and 0x82 entries respectively); Apple's fsck_exfat refuses to verify
// a volume that's missing either of them.
const (
	fmtBytesPerSectorShift    = 9 // 512-byte sectors
	fmtSectorsPerClusterShift = 3 // 8 sectors/cluster → 4 KiB clusters
	fmtFATOffset              = 24
	fmtNumberOfFATs           = 1
	fmtBitmapCluster          = 2
	fmtUpcaseCluster          = 3
	fmtRootDirCluster         = 4

	// fmtFATGrowthHeadroom controls how much spare FAT space Format
	// provisions so that subsequent Grow calls don't immediately hit
	// the FAT-capacity ceiling. The FAT is sized to cover the max
	// addressable cluster count for fmtFATGrowthHeadroom × sizeBytes.
	fmtFATGrowthHeadroom = 4
)

type formatFile interface {
	WriteAt([]byte, int64) (int, error)
	Truncate(int64) error
	Close() error
}

var formatOpenFile = func(path string) (formatFile, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
}

var formatRandUint32 = func() uint32 {
	return rand.Uint32()
}

var formatOpenFS = Open

// Format creates a new exFAT filesystem in the file at path.
// The file is created (or truncated) and formatted. sizeBytes must be a
// multiple of the cluster size (4096) and large enough to hold the metadata
// region plus at least one data cluster.
//
// On success the newly formatted filesystem is opened and returned; the
// caller must Close it when done.
func Format(path string, sizeBytes int64, cfg FormatConfig) (filesystem.Filesystem, error) {
	const bytesPerSector = 1 << fmtBytesPerSectorShift
	const sectorsPerCluster = 1 << fmtSectorsPerClusterShift
	const clusterSize = bytesPerSector * sectorsPerCluster

	if sizeBytes%clusterSize != 0 {
		return nil, fmt.Errorf("exfat: format: size %d is not a multiple of cluster size %d",
			sizeBytes, clusterSize)
	}

	totalSectors := uint64(sizeBytes) / bytesPerSector

	// Provision a FAT generous enough to accommodate Grow up to
	// fmtFATGrowthHeadroom × sizeBytes without relocating the cluster
	// heap. The minimum FAT length covers the max addressable cluster
	// count for the headroom size; we round up to a whole cluster so
	// the heap offset stays cluster-aligned.
	headroomSectors := totalSectors * fmtFATGrowthHeadroom
	maxClustersForHeadroom := (headroomSectors - fmtFATOffset) / sectorsPerCluster
	fatBytes := (maxClustersForHeadroom + 2) * 4
	fatLength := uint64((fatBytes + bytesPerSector - 1) / bytesPerSector)
	if fatLength < 8 {
		fatLength = 8
	}
	// Round the cluster heap offset up to a cluster boundary past the
	// FAT. This keeps cluster reads aligned, which Apple's fsck_exfat
	// requires.
	clusterHeapOffset := uint64(fmtFATOffset) + fatLength
	if rem := clusterHeapOffset % sectorsPerCluster; rem != 0 {
		clusterHeapOffset += sectorsPerCluster - rem
	}

	if totalSectors <= clusterHeapOffset {
		return nil, fmt.Errorf("exfat: format: size %d too small", sizeBytes)
	}
	clusterCount := uint32((totalSectors - clusterHeapOffset) / sectorsPerCluster)
	if clusterCount < 1 {
		return nil, fmt.Errorf("exfat: format: size %d too small", sizeBytes)
	}
	// System-file layout: the allocation bitmap (one bit per data cluster,
	// possibly spanning several clusters for large volumes) starts at cluster
	// 2, followed by the up-case table and the root directory.
	bitmapSizeBytes := (uint64(clusterCount) + 7) / 8
	bitmapClusters := uint32((bitmapSizeBytes + uint64(clusterSize) - 1) / uint64(clusterSize))
	bitmapCluster := uint32(fmtBitmapCluster)
	upcaseCluster := bitmapCluster + bitmapClusters
	rootDirCluster := upcaseCluster + 1
	sysClusters := bitmapClusters + 2 // bitmap chain + upcase + root
	if clusterCount < sysClusters {
		return nil, fmt.Errorf("exfat: format: size %d leaves only %d data clusters; need ≥%d for bitmap+upcase+root", sizeBytes, clusterCount, sysClusters)
	}

	// dataClusterOffset returns the absolute byte offset of the start of
	// cluster c within the cluster heap. Cluster indices are 2-based.
	dataClusterOffset := func(c uint32) int64 {
		return int64(clusterHeapOffset)*bytesPerSector + int64(c-2)*clusterSize
	}

	serialNumber := cfg.VolumeSerialNumber
	if serialNumber == 0 {
		serialNumber = formatRandUint32()
		if serialNumber == 0 {
			serialNumber = 0xDEADBEEF
		}
	}

	f, err := formatOpenFile(path)
	if err != nil {
		return nil, fmt.Errorf("exfat: format: %w", err)
	}
	if err := f.Truncate(sizeBytes); err != nil {
		f.Close()
		return nil, fmt.Errorf("exfat: format: truncate: %w", err)
	}

	le := binary.LittleEndian

	// ── Main boot region (sector 0) ───────────────────────────────────────────
	// exFAT boot sector layout (512 bytes):
	//   [0:3]   JumpBoot
	//   [3:11]  OEM name "EXFAT   "
	//   [11:64] MustBeZero
	//   [64:72] PartitionStartSector (0 for non-partitioned)
	//   [72:80] VolumeLength (sectors)
	//   [80:84] FATOffset (sectors)
	//   [84:88] FATLength (sectors)
	//   [88:92] ClusterHeapOffset (sectors)
	//   [92:96] ClusterCount
	//   [96:100]  RootDirectoryCluster
	//   [100:104] VolumeSerialNumber
	//   [104:106] FileSystemRevision
	//   [106:108] VolumeFlags
	//   [108]   BytesPerSectorShift
	//   [109]   SectorsPerClusterShift
	//   [110]   NumberOfFATs
	//   [111]   DriveSelect
	//   [112]   PercentInUse (0xFF = unknown)
	//   [113:510] MustBeZero/BootCode
	//   [510:512] BootSignature 0x55 0xAA
	boot := make([]byte, bytesPerSector)
	boot[0] = 0xEB
	boot[1] = 0x76
	boot[2] = 0x90
	copy(boot[3:11], []byte("EXFAT   "))
	// [11:64] zero (MustBeZero)
	// PartitionStartSector = 0
	le.PutUint64(boot[72:], totalSectors)
	le.PutUint32(boot[80:], fmtFATOffset)
	le.PutUint32(boot[84:], uint32(fatLength))
	le.PutUint32(boot[88:], uint32(clusterHeapOffset))
	le.PutUint32(boot[92:], clusterCount)
	le.PutUint32(boot[96:], rootDirCluster)
	le.PutUint32(boot[100:], serialNumber)
	le.PutUint16(boot[104:], 0x0100) // revision 1.00
	// VolumeFlags = 0, DriveSelect = 0x80
	boot[108] = fmtBytesPerSectorShift
	boot[109] = fmtSectorsPerClusterShift
	boot[110] = fmtNumberOfFATs
	boot[111] = 0x80 // DriveSelect
	boot[112] = 0xFF // PercentInUse: unknown
	boot[510] = 0x55
	boot[511] = 0xAA
	if _, err := f.WriteAt(boot, 0); err != nil {
		f.Close()
		return nil, fmt.Errorf("exfat: format: write boot sector: %w", err)
	}

	// ── Extended boot sectors (sectors 1..8) ─────────────────────────────────
	// Per the Microsoft exFAT specification (section 3.2), the eight sectors
	// immediately following the Main Boot Sector are the Extended Boot
	// Sectors. They are reserved for extended boot code and must end with
	// the ExtendedBootSignature 0xAA550000 (little-endian: 00 00 55 AA at
	// bytes 508..511). The first 508 bytes may be left zeroed by formatters
	// that don't ship extended boot code — which is exactly what we do.
	extBoot := make([]byte, bytesPerSector)
	extBoot[508] = 0x00
	extBoot[509] = 0x00
	extBoot[510] = 0x55
	extBoot[511] = 0xAA
	for sector := int64(1); sector <= 8; sector++ {
		if _, err := f.WriteAt(extBoot, sector*bytesPerSector); err != nil {
			f.Close()
			return nil, fmt.Errorf("exfat: format: write extended boot sector %d: %w", sector, err)
		}
	}

	// ── OEM Parameters (sector 9) and Reserved (sector 10) ──────────────────
	// Both default to all-zero; OEM Parameters is a sequence of 10 GUID-keyed
	// records and we don't emit any. The Reserved sector must be zero.
	zeroSector := make([]byte, bytesPerSector)
	for _, sector := range []int64{9, 10} {
		if _, err := f.WriteAt(zeroSector, sector*bytesPerSector); err != nil {
			f.Close()
			return nil, fmt.Errorf("exfat: format: write boot-region sector %d: %w", sector, err)
		}
	}

	// ── Boot Checksum (sector 11) ───────────────────────────────────────────
	// Sector 11 holds the BootChecksum: a 32-bit hash of bytes 0..(11·sector-1)
	// of the boot region, EXCLUDING bytes 106, 107 (VolumeFlags) and 112
	// (PercentInUse) of the Main Boot Sector. The 32-bit value is repeated
	// once per 4 bytes across the full sector.
	mainBoot := boot
	bootRegion := make([]byte, 11*bytesPerSector)
	copy(bootRegion[0*bytesPerSector:], mainBoot)
	for sector := 1; sector <= 8; sector++ {
		copy(bootRegion[sector*bytesPerSector:], extBoot)
	}
	// sectors 9 and 10 already zero in bootRegion.
	checksum := exfatBootChecksum(bootRegion)
	checksumSector := make([]byte, bytesPerSector)
	for offset := 0; offset < bytesPerSector; offset += 4 {
		le.PutUint32(checksumSector[offset:], checksum)
	}
	if _, err := f.WriteAt(checksumSector, 11*bytesPerSector); err != nil {
		f.Close()
		return nil, fmt.Errorf("exfat: format: write boot checksum sector: %w", err)
	}

	// ── Backup boot region (sectors 12..23) ─────────────────────────────────
	// The backup boot region is a verbatim copy of the main boot region.
	// Apple's fsck_exfat falls back to it when the main region fails to
	// validate, so we must mirror every sector — not just the boot sector.
	if _, err := f.WriteAt(mainBoot, 12*bytesPerSector); err != nil {
		f.Close()
		return nil, fmt.Errorf("exfat: format: write backup main boot sector: %w", err)
	}
	for sector := int64(1); sector <= 8; sector++ {
		if _, err := f.WriteAt(extBoot, (12+sector)*bytesPerSector); err != nil {
			f.Close()
			return nil, fmt.Errorf("exfat: format: write backup extended boot sector %d: %w", sector, err)
		}
	}
	for _, sector := range []int64{9, 10} {
		if _, err := f.WriteAt(zeroSector, (12+sector)*bytesPerSector); err != nil {
			f.Close()
			return nil, fmt.Errorf("exfat: format: write backup boot-region sector %d: %w", sector, err)
		}
	}
	if _, err := f.WriteAt(checksumSector, (12+11)*bytesPerSector); err != nil {
		f.Close()
		return nil, fmt.Errorf("exfat: format: write backup boot checksum sector: %w", err)
	}

	// ── FAT ───────────────────────────────────────────────────────────────────
	// FAT[0] = 0xFFFFFFF8 (media type), FAT[1] = 0xFFFFFFFF (reserved). The
	// allocation bitmap occupies a contiguous chain of bitmapClusters clusters
	// starting at cluster 2; the up-case table and root directory follow, each
	// a single cluster.
	fatBuf := make([]byte, int(rootDirCluster+1)*4)
	le.PutUint32(fatBuf[0:], 0xFFFFFFF8) // FAT[0]
	le.PutUint32(fatBuf[4:], 0xFFFFFFFF) // FAT[1]
	for c := bitmapCluster; c < bitmapCluster+bitmapClusters; c++ {
		if c == bitmapCluster+bitmapClusters-1 {
			le.PutUint32(fatBuf[c*4:], 0xFFFFFFFF) // last bitmap cluster: EOC
		} else {
			le.PutUint32(fatBuf[c*4:], c+1) // chain to the next bitmap cluster
		}
	}
	le.PutUint32(fatBuf[upcaseCluster*4:], 0xFFFFFFFF)  // up-case EOC
	le.PutUint32(fatBuf[rootDirCluster*4:], 0xFFFFFFFF) // root EOC
	fatOff := int64(fmtFATOffset) * bytesPerSector
	if _, err := f.WriteAt(fatBuf, fatOff); err != nil {
		f.Close()
		return nil, fmt.Errorf("exfat: format: write FAT: %w", err)
	}

	// ── Allocation Bitmap (clusters 2..) ─────────────────────────────────────
	// One bit per data cluster, bit 0 == cluster 2. Mark the system clusters
	// (the bitmap chain itself, the up-case table, and the root directory)
	// allocated; everything else is free. The bitmap may span several clusters.
	bitmap := make([]byte, int(bitmapClusters)*int(clusterSize))
	for i := uint32(0); i < sysClusters; i++ {
		bitmap[i/8] |= 1 << (i % 8)
	}
	bitmapOff := dataClusterOffset(bitmapCluster)
	if _, err := f.WriteAt(bitmap, bitmapOff); err != nil {
		f.Close()
		return nil, fmt.Errorf("exfat: format: write allocation bitmap: %w", err)
	}

	// ── Up-case Table (cluster 3) ────────────────────────────────────────────
	// We emit a minimal *compressed* up-case table covering ASCII a..z → A..Z
	// (the only mapping our NameHash routine relies on; non-ASCII filenames
	// are passed through verbatim by Go's strings.ToUpper too).
	//
	// Compressed format (Microsoft exFAT spec section 7.2.5.1):
	//   * 0xFFFF N   means N consecutive identity entries (skip).
	//   * any other 16-bit value V at index i means "code unit i upcases to V".
	//
	// Layout:
	//   FFFF 0061     : identity for code units 0x0000..0x0060
	//   0041..005A    : explicit mappings 'a'..'z' → 'A'..'Z' (26 entries)
	//   FFFF FF85     : identity for code units 0x007B..0xFFFF
	upcaseTable := buildExfatUpcaseTable()
	upcaseChecksum := exfatTableChecksum(upcaseTable)
	upcaseClusterBuf := make([]byte, clusterSize)
	copy(upcaseClusterBuf, upcaseTable)
	upcaseOff := dataClusterOffset(upcaseCluster)
	if _, err := f.WriteAt(upcaseClusterBuf, upcaseOff); err != nil {
		f.Close()
		return nil, fmt.Errorf("exfat: format: write up-case table: %w", err)
	}

	// ── Root Directory (cluster 4) ───────────────────────────────────────────
	// Mandatory root entries for an exFAT volume: Allocation Bitmap (0x81),
	// Up-case Table (0x82), and (optionally) Volume Label (0x83). Volume
	// Label is added later by SetLabel if cfg.Label is set, but we leave
	// space for it by writing the bitmap+upcase pair first.
	rootBuf := make([]byte, clusterSize)
	// Allocation Bitmap entry: type 0x81.
	rootBuf[0] = 0x81
	// BitmapFlags = 0 (first FAT)
	le.PutUint32(rootBuf[20:], bitmapCluster)   // FirstCluster
	le.PutUint64(rootBuf[24:], bitmapSizeBytes) // DataLength
	// Up-case Table entry: type 0x82.
	rootBuf[dirEntrySize+0] = 0x82
	le.PutUint32(rootBuf[dirEntrySize+4:], upcaseChecksum) // TableChecksum
	le.PutUint32(rootBuf[dirEntrySize+20:], upcaseCluster)
	le.PutUint64(rootBuf[dirEntrySize+24:], uint64(len(upcaseTable)))
	rootOff := dataClusterOffset(rootDirCluster)
	if _, err := f.WriteAt(rootBuf, rootOff); err != nil {
		f.Close()
		return nil, fmt.Errorf("exfat: format: write root directory: %w", err)
	}

	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("exfat: format: close: %w", err)
	}

	fs, err := formatOpenFS(path, -1)
	if err != nil {
		return nil, err
	}
	if cfg.Label != "" {
		// formatOpenFS returns filesystem.Filesystem; the Labeller
		// capability is the typed gateway to SetLabel.
		l, ok := fs.(filesystem.Labeller)
		if !ok {
			fs.Close()
			return nil, fmt.Errorf("exfat: driver does not satisfy filesystem.Labeller")
		}
		if err := l.SetLabel(cfg.Label); err != nil {
			fs.Close()
			return nil, fmt.Errorf("exfat: seed label: %w", err)
		}
	}
	return fs, nil
}

// buildExfatUpcaseTable returns the minimal compressed Up-case Table that
// matches the case-folding our NameHash uses (Go's strings.ToUpper on ASCII):
// codepoints 'a'..'z' upper-case to 'A'..'Z', everything else maps to itself.
//
// The compressed format encodes a run of identity mappings as the literal
// 0xFFFF followed by a 16-bit count. Anything else is an explicit mapping:
// "code unit at index i upcases to the stored value".
func buildExfatUpcaseTable() []byte {
	const totalCodeUnits = 0x10000
	const lowerStart = 0x61 // 'a'
	const upperStart = 0x41 // 'A'
	const lowerCount = 26
	const tailStart = lowerStart + lowerCount // 0x7B

	// Entries (each = 2 bytes LE):
	//   FFFF lowerStart                     skip 0x0000..0x0060
	//   upperStart..upperStart+lowerCount-1 explicit 'a'..'z' -> 'A'..'Z'
	//   FFFF (totalCodeUnits-tailStart)     skip 0x007B..0xFFFF
	entries := make([]uint16, 0, 4+lowerCount)
	entries = append(entries, 0xFFFF, uint16(lowerStart))
	for i := 0; i < lowerCount; i++ {
		entries = append(entries, uint16(upperStart+i))
	}
	entries = append(entries, 0xFFFF, uint16(totalCodeUnits-tailStart))

	out := make([]byte, len(entries)*2)
	for i, v := range entries {
		out[2*i] = byte(v)
		out[2*i+1] = byte(v >> 8)
	}
	return out
}

// exfatTableChecksum computes the 32-bit checksum of an arbitrary byte slice
// using the same rotate-add algorithm as exfatBootChecksum, but without the
// VolumeFlags/PercentInUse exclusions. The Up-case Table directory entry
// stores this checksum in its TableChecksum field; fsck implementations
// verify the table contents against it.
func exfatTableChecksum(data []byte) uint32 {
	var sum uint32
	for _, b := range data {
		var carry uint32
		if sum&1 != 0 {
			carry = 0x80000000
		}
		sum = carry + (sum >> 1) + uint32(b)
	}
	return sum
}

// exfatBootChecksum computes the 32-bit Boot Checksum value defined in
// section 3.4 of the Microsoft exFAT specification. The hash is taken
// over the first 11 sectors of the boot region, EXCLUDING the three
// bytes that the spec lets the filesystem mutate without resealing
// the checksum: VolumeFlags (bytes 106..107) and PercentInUse (byte 112)
// of the Main Boot Sector.
//
// The single-byte step is:
//
//	checksum = (rotateRight32(checksum, 1)) + byte
//
// or, equivalently:
//
//	if checksum&1 != 0 { checksum = 0x80000000 } else { checksum = 0 }
//	checksum += (oldChecksum >> 1) + byte
func exfatBootChecksum(region []byte) uint32 {
	const (
		volumeFlagsLo  = 106
		volumeFlagsHi  = 107
		percentInUseAt = 112
	)
	var sum uint32
	for index, b := range region {
		if index == volumeFlagsLo || index == volumeFlagsHi || index == percentInUseAt {
			continue
		}
		var carry uint32
		if sum&1 != 0 {
			carry = 0x80000000
		}
		sum = carry + (sum >> 1) + uint32(b)
	}
	return sum
}
