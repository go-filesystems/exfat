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
const (
	fmtBytesPerSectorShift    = 9  // 512-byte sectors
	fmtSectorsPerClusterShift = 3  // 8 sectors/cluster → 4 KiB clusters
	fmtFATOffset              = 24 // sectors from partition start (12 KiB)
	fmtFATLength              = 8  // sectors for FAT (covers up to ~8 K clusters)
	fmtClusterHeapOffset      = 32 // sectors from partition start (16 KiB)
	fmtNumberOfFATs           = 1
	fmtRootDirCluster         = 2
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
	clusterCount := uint32((totalSectors - fmtClusterHeapOffset) / sectorsPerCluster)
	if clusterCount < 1 {
		return nil, fmt.Errorf("exfat: format: size %d too small", sizeBytes)
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
	le.PutUint32(boot[84:], fmtFATLength)
	le.PutUint32(boot[88:], fmtClusterHeapOffset)
	le.PutUint32(boot[92:], clusterCount)
	le.PutUint32(boot[96:], fmtRootDirCluster)
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
	// Backup boot sector at sector 12.
	if _, err := f.WriteAt(boot, 12*bytesPerSector); err != nil {
		f.Close()
		return nil, fmt.Errorf("exfat: format: write backup boot sector: %w", err)
	}

	// ── FAT ───────────────────────────────────────────────────────────────────
	// FAT[0] = 0xFFFFFFF8 (media type), FAT[1] = 0xFFFFFFFF (reserved)
	// FAT[2] = root directory cluster EOC.
	fatBuf := make([]byte, 12)
	le.PutUint32(fatBuf[0:], 0xFFFFFFF8)
	le.PutUint32(fatBuf[4:], 0xFFFFFFFF)
	le.PutUint32(fatBuf[8:], 0xFFFFFFFF)
	fatOff := int64(fmtFATOffset) * bytesPerSector
	if _, err := f.WriteAt(fatBuf, fatOff); err != nil {
		f.Close()
		return nil, fmt.Errorf("exfat: format: write FAT: %w", err)
	}

	// ── Root directory cluster (cluster 2) ────────────────────────────────────
	// An empty exFAT root directory has no mandatory entries (unlike FAT32).
	// We just leave the cluster zeroed (first entry type 0x00 → end of directory).

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
