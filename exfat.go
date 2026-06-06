package filesystem_exfat

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf16"

	filesystem "github.com/go-filesystems/interface"
)

const (
	sectorSize        = 512
	dirEntrySize      = 32
	exfatEntryEnd     = 0x00
	exfatEntryFile    = 0x85
	exfatEntryStream  = 0xC0
	exfatEntryName    = 0xC1
	exfatAttrReadOnly = 0x01
	exfatAttrDir      = 0x10
	nameCharsPerEntry = 15
	exfatModeDir      = 0o040755
	exfatModeDirRO    = 0o040555
	exfatModeFile     = 0o100644
	exfatModeFileRO   = 0o100444
)

type rootDirEntry struct {
	name    string
	attr    uint16
	cluster uint32
	size    uint64
}

// Info holds the fields decoded from the exFAT main boot sector.
type Info struct {
	PartitionStartSector   uint64
	VolumeLength           uint64
	FATOffset              uint32
	FATLength              uint32
	ClusterHeapOffset      uint32
	ClusterCount           uint32
	RootDirectoryCluster   uint32
	VolumeSerialNumber     uint32
	FileSystemRevision     uint16
	VolumeFlags            uint16
	BytesPerSectorShift    uint8
	SectorsPerClusterShift uint8
	NumberOfFATs           uint8
	DriveSelect            uint8
	PercentInUse           uint8
}

// BytesPerSector returns the logical sector size in bytes.
func (info Info) BytesPerSector() uint32 {
	return 1 << info.BytesPerSectorShift
}

// SectorsPerCluster returns the cluster size expressed in sectors.
func (info Info) SectorsPerCluster() uint32 {
	return 1 << info.SectorsPerClusterShift
}

// ClusterSize returns the allocation cluster size in bytes.
func (info Info) ClusterSize() uint64 {
	return uint64(info.BytesPerSector()) * uint64(info.SectorsPerCluster())
}

// FATOffsetBytes returns the absolute byte offset of the first FAT.
func (info Info) FATOffsetBytes(partOffset int64) int64 {
	return partOffset + int64(info.FATOffset)*int64(info.BytesPerSector())
}

// ClusterHeapOffsetBytes returns the absolute byte offset of the cluster heap.
func (info Info) ClusterHeapOffsetBytes(partOffset int64) int64 {
	return partOffset + int64(info.ClusterHeapOffset)*int64(info.BytesPerSector())
}

// RootDirOffset returns the absolute byte offset of the root directory cluster.
func (info Info) RootDirOffset(partOffset int64) int64 {
	return info.ClusterHeapOffsetBytes(partOffset) + int64(info.RootDirectoryCluster-2)*int64(info.ClusterSize())
}

// diskRW combines the read, write, and close operations needed by FS.
type diskRW interface {
	io.ReaderAt
	io.WriterAt
	io.Closer
}

// FS represents an opened exFAT image.
type exfatFS struct {
	f          diskRW
	partOffset int64
	info       Info
	label      string
}

var (
	openFile            = os.OpenFile
	openPartitionOffset = partitionOffset
	openReadInfo        = readInfo
)

// Verify implementation of the common filesystem interface.
var _ filesystem.Filesystem = (*exfatFS)(nil)

// Open opens imagePath, optionally selecting a partition, and parses the exFAT boot sector.
func Open(imagePath string, partIndex int) (filesystem.Filesystem, error) {
	f, err := openFile(imagePath, os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("exfat: open %s: %w", imagePath, err)
	}
	off, err := openPartitionOffset(f, partIndex)
	if err != nil {
		f.Close()
		return nil, err
	}
	info, err := openReadInfo(f, off)
	if err != nil {
		f.Close()
		return nil, err
	}
	fs := &exfatFS{f: f, partOffset: off, info: info}
	// Best-effort volume-label read. A malformed label entry shouldn't
	// fail Open — it just means Label() returns "".
	if lbl, err := readVolumeLabel(f, info, off); err == nil {
		fs.label = lbl
	}
	return fs, nil
}

// Close releases the underlying file handle.
func (fs *exfatFS) Close() error { return fs.f.Close() }

// Info returns the decoded boot-sector metadata.
func (fs *exfatFS) Info() Info { return fs.info }

// PartitionOffset returns the byte offset of the selected partition.
func (fs *exfatFS) PartitionOffset() int64 { return fs.partOffset }

// Stat returns basic metadata for the root directory or any entry at path.
func (fs *exfatFS) Stat(path string) (filesystem.Stat, error) {
	entry, _, err := fs.resolvePath(path)
	if err != nil {
		return nil, err
	}
	return filesystem.NewStat(entry.mode(), entry.size, uint64(entry.cluster)), nil
}

// ListDir lists the entries of the directory at path (any depth).
func (fs *exfatFS) ListDir(path string) ([]filesystem.DirEntry, error) {
	entry, _, err := fs.resolvePath(path)
	if err != nil {
		return nil, err
	}
	if entry.attr&exfatAttrDir == 0 {
		return nil, fmt.Errorf("exfat: %q is not a directory", path)
	}
	buf, err := fs.readDirBuf(entry.cluster)
	if err != nil {
		return nil, err
	}
	return parseRootDirEntries(buf)
}

// ReadFile reads and returns the contents of the regular file at path.
func (fs *exfatFS) ReadFile(path string) ([]byte, error) {
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
	return fs.readClusterChain(entry.cluster, entry.size)
}

// WriteFile creates or overwrites the regular file at path with data and permission bits.
func (fs *exfatFS) WriteFile(path string, data []byte, perm os.FileMode) error {
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
	existingOff, existingSec := exfatFindEntry(rootBuf, name)
	if existingOff >= 0 {
		stream := rootBuf[existingOff+dirEntrySize : existingOff+2*dirEntrySize]
		oldCluster := binary.LittleEndian.Uint32(stream[20:24])
		if oldCluster >= 2 {
			if err := fs.freeChain(oldCluster); err != nil {
				return err
			}
		}
		exfatDeleteEntry(rootBuf, existingOff, existingSec)
	}
	var firstCluster uint32
	if len(data) > 0 {
		firstCluster, err = fs.writeData(data)
		if err != nil {
			return err
		}
	}
	nameWords := utf16.Encode([]rune(name))
	numNameEntries := (len(nameWords) + nameCharsPerEntry - 1) / nameCharsPerEntry
	freeOff := exfatFindFreeSlot(rootBuf, 2+numNameEntries)
	if freeOff < 0 {
		return fmt.Errorf("exfat: directory is full")
	}
	var attrs uint16 = 0x20 // archive
	if perm&0o200 == 0 {
		attrs |= uint16(exfatAttrReadOnly)
	}
	copy(rootBuf[freeOff:], makeExFATEntrySet(name, attrs, firstCluster, uint64(len(data))))
	return fs.writeDirBuf(parentCluster, rootBuf)
}

// ReadLink always returns an error; exFAT does not support symbolic links.
func (fs *exfatFS) ReadLink(path string) (string, error) {
	return "", fmt.Errorf("exfat: %q is not a symbolic link", path)
}

// MkDir creates a new empty directory at path.
func (fs *exfatFS) MkDir(path string, perm os.FileMode) error {
	name, parentCluster, err := fs.getParentDir(path)
	if err != nil {
		return err
	}
	rootBuf, err := fs.readDirBuf(parentCluster)
	if err != nil {
		return err
	}
	if off, _ := exfatFindEntry(rootBuf, name); off >= 0 {
		return fmt.Errorf("exfat: %q already exists", path)
	}
	nameWords := utf16.Encode([]rune(name))
	numNameEntries := (len(nameWords) + nameCharsPerEntry - 1) / nameCharsPerEntry
	freeOff := exfatFindFreeSlot(rootBuf, 2+numNameEntries)
	if freeOff < 0 {
		return fmt.Errorf("exfat: directory is full")
	}
	cluster, err := fs.allocCluster()
	if err != nil {
		return err
	}
	if err := fs.setFATEntry(cluster, 0xFFFFFFFF); err != nil {
		return err
	}
	clusterBuf := make([]byte, fs.info.ClusterSize())
	clusterOff := fs.info.ClusterHeapOffsetBytes(fs.partOffset) + int64(cluster-2)*int64(fs.info.ClusterSize())
	if _, err := fs.f.WriteAt(clusterBuf, clusterOff); err != nil {
		return fmt.Errorf("exfat: write directory cluster: %w", err)
	}
	var attrs uint16 = uint16(exfatAttrDir)
	if perm&0o200 == 0 {
		attrs |= uint16(exfatAttrReadOnly)
	}
	copy(rootBuf[freeOff:], makeExFATEntrySet(name, attrs, cluster, 0))
	return fs.writeDirBuf(parentCluster, rootBuf)
}

// DeleteFile removes the regular file at path, freeing its cluster chain.
func (fs *exfatFS) DeleteFile(path string) error {
	name, parentCluster, err := fs.getParentDir(path)
	if err != nil {
		return err
	}
	rootBuf, err := fs.readDirBuf(parentCluster)
	if err != nil {
		return err
	}
	entryOff, entrySec := exfatFindEntry(rootBuf, name)
	if entryOff < 0 {
		return fmt.Errorf("exfat: %q not found", path)
	}
	attrs := binary.LittleEndian.Uint16(rootBuf[entryOff+4 : entryOff+6])
	if attrs&uint16(exfatAttrDir) != 0 {
		return fmt.Errorf("exfat: %q is a directory", path)
	}
	stream := rootBuf[entryOff+dirEntrySize : entryOff+2*dirEntrySize]
	firstCluster := binary.LittleEndian.Uint32(stream[20:24])
	if firstCluster >= 2 {
		if err := fs.freeChain(firstCluster); err != nil {
			return err
		}
	}
	exfatDeleteEntry(rootBuf, entryOff, entrySec)
	return fs.writeDirBuf(parentCluster, rootBuf)
}

// deleteAllContents recursively removes all files and subdirectories inside the
// directory at dirCluster, freeing their cluster chains. The directory cluster
// itself is not freed; that is the caller's responsibility.
func (fs *exfatFS) deleteAllContents(dirCluster uint32) error {
	buf, err := fs.readDirBuf(dirCluster)
	if err != nil {
		return err
	}
	le := binary.LittleEndian
	for offset := 0; offset+dirEntrySize <= len(buf); offset += dirEntrySize {
		entry := buf[offset : offset+dirEntrySize]
		typ := entry[0]
		if typ == exfatEntryEnd {
			break
		}
		if typ != exfatEntryFile {
			continue
		}
		secondaryCount := int(entry[1])
		if secondaryCount < 2 || offset+(secondaryCount+1)*dirEntrySize > len(buf) {
			continue
		}
		stream := buf[offset+dirEntrySize : offset+2*dirEntrySize]
		if stream[0] != exfatEntryStream {
			continue
		}
		attrs := le.Uint16(entry[4:6])
		cluster := le.Uint32(stream[20:24])
		if attrs&uint16(exfatAttrDir) != 0 && cluster >= 2 {
			if err := fs.deleteAllContents(cluster); err != nil {
				return err
			}
		}
		if cluster >= 2 {
			if err := fs.freeChain(cluster); err != nil {
				return err
			}
		}
		offset += secondaryCount * dirEntrySize
	}
	return nil
}

// DeleteDir removes the directory at path, recursively deleting any contents.
func (fs *exfatFS) DeleteDir(path string) error {
	name, parentCluster, err := fs.getParentDir(path)
	if err != nil {
		return err
	}
	rootBuf, err := fs.readDirBuf(parentCluster)
	if err != nil {
		return err
	}
	entryOff, entrySec := exfatFindEntry(rootBuf, name)
	if entryOff < 0 {
		return fmt.Errorf("exfat: %q not found", path)
	}
	attrs := binary.LittleEndian.Uint16(rootBuf[entryOff+4 : entryOff+6])
	if attrs&uint16(exfatAttrDir) == 0 {
		return fmt.Errorf("exfat: %q is not a directory", path)
	}
	stream := rootBuf[entryOff+dirEntrySize : entryOff+2*dirEntrySize]
	firstCluster := binary.LittleEndian.Uint32(stream[20:24])
	if firstCluster >= 2 {
		if err := fs.deleteAllContents(firstCluster); err != nil {
			return err
		}
		if err := fs.freeChain(firstCluster); err != nil {
			return err
		}
	}
	exfatDeleteEntry(rootBuf, entryOff, entrySec)
	return fs.writeDirBuf(parentCluster, rootBuf)
}

// Rename moves the entry at oldPath to newPath.
// If newPath already exists it is replaced.
func (fs *exfatFS) Rename(oldPath, newPath string) error {
	oldName, oldParentCluster, err := fs.getParentDir(oldPath)
	if err != nil {
		return err
	}
	newName, newParentCluster, err := fs.getParentDir(newPath)
	if err != nil {
		return err
	}
	if oldParentCluster == newParentCluster && strings.EqualFold(oldName, newName) {
		return nil
	}
	// Read source parent directory
	oldBuf, err := fs.readDirBuf(oldParentCluster)
	if err != nil {
		return err
	}
	oldOff, oldSec := exfatFindEntry(oldBuf, oldName)
	if oldOff < 0 {
		return fmt.Errorf("exfat: %q not found", oldPath)
	}
	oldAttrs := binary.LittleEndian.Uint16(oldBuf[oldOff+4 : oldOff+6])
	oldStream := oldBuf[oldOff+dirEntrySize : oldOff+2*dirEntrySize]
	oldCluster := binary.LittleEndian.Uint32(oldStream[20:24])
	oldSize := binary.LittleEndian.Uint64(oldStream[24:32])

	// Read destination parent directory (may be same as source)
	var newBuf []byte
	if newParentCluster == oldParentCluster {
		newBuf = oldBuf
	} else {
		newBuf, err = fs.readDirBuf(newParentCluster)
		if err != nil {
			return err
		}
	}
	newOff, newSec := exfatFindEntry(newBuf, newName)
	if newOff >= 0 {
		newStream := newBuf[newOff+dirEntrySize : newOff+2*dirEntrySize]
		newCluster := binary.LittleEndian.Uint32(newStream[20:24])
		if newCluster >= 2 {
			if err := fs.freeChain(newCluster); err != nil {
				return err
			}
		}
		exfatDeleteEntry(newBuf, newOff, newSec)
	}

	if newParentCluster == oldParentCluster {
		// Recalculate oldOff after potential delete above
		oldOff, oldSec = exfatFindEntry(oldBuf, oldName)
		oldAttrs = binary.LittleEndian.Uint16(oldBuf[oldOff+4 : oldOff+6])
		oldStream = oldBuf[oldOff+dirEntrySize : oldOff+2*dirEntrySize]
		oldCluster = binary.LittleEndian.Uint32(oldStream[20:24])
		oldSize = binary.LittleEndian.Uint64(oldStream[24:32])
		exfatDeleteEntry(oldBuf, oldOff, oldSec)
		nameWords := utf16.Encode([]rune(newName))
		numNameEntries := (len(nameWords) + nameCharsPerEntry - 1) / nameCharsPerEntry
		freeOff := exfatFindFreeSlot(oldBuf, 2+numNameEntries)
		if freeOff < 0 {
			return fmt.Errorf("exfat: directory is full")
		}
		copy(oldBuf[freeOff:], makeExFATEntrySet(newName, oldAttrs, oldCluster, oldSize))
		return fs.writeDirBuf(oldParentCluster, oldBuf)
	}

	// Cross-directory rename
	exfatDeleteEntry(oldBuf, oldOff, oldSec)
	if err := fs.writeDirBuf(oldParentCluster, oldBuf); err != nil {
		return err
	}
	nameWords := utf16.Encode([]rune(newName))
	numNameEntries := (len(nameWords) + nameCharsPerEntry - 1) / nameCharsPerEntry
	freeOff := exfatFindFreeSlot(newBuf, 2+numNameEntries)
	if freeOff < 0 {
		return fmt.Errorf("exfat: destination directory is full")
	}
	copy(newBuf[freeOff:], makeExFATEntrySet(newName, oldAttrs, oldCluster, oldSize))
	return fs.writeDirBuf(newParentCluster, newBuf)
}

func readInfo(r io.ReaderAt, partOffset int64) (Info, error) {
	buf := make([]byte, sectorSize)
	if _, err := r.ReadAt(buf, partOffset); err != nil {
		return Info{}, fmt.Errorf("exfat: read boot sector: %w", err)
	}
	if buf[510] != 0x55 || buf[511] != 0xAA {
		return Info{}, fmt.Errorf("exfat: invalid boot sector signature")
	}
	if string(buf[3:11]) != "EXFAT   " {
		return Info{}, fmt.Errorf("exfat: invalid filesystem name %q", string(buf[3:11]))
	}

	le := binary.LittleEndian
	volumeLength := le.Uint64(buf[72:])
	if volumeLength == 0 {
		return Info{}, fmt.Errorf("exfat: volume length is zero")
	}
	fatOffset := le.Uint32(buf[80:])
	if fatOffset == 0 {
		return Info{}, fmt.Errorf("exfat: FAT offset is zero")
	}
	fatLength := le.Uint32(buf[84:])
	if fatLength == 0 {
		return Info{}, fmt.Errorf("exfat: FAT length is zero")
	}
	clusterHeapOffset := le.Uint32(buf[88:])
	if clusterHeapOffset == 0 {
		return Info{}, fmt.Errorf("exfat: cluster heap offset is zero")
	}
	clusterCount := le.Uint32(buf[92:])
	if clusterCount == 0 {
		return Info{}, fmt.Errorf("exfat: cluster count is zero")
	}
	rootDirectoryCluster := le.Uint32(buf[96:])
	if rootDirectoryCluster < 2 || rootDirectoryCluster > clusterCount+1 {
		return Info{}, fmt.Errorf("exfat: invalid root directory cluster %d", rootDirectoryCluster)
	}
	bytesPerSectorShift := buf[108]
	if bytesPerSectorShift < 9 || bytesPerSectorShift > 12 {
		return Info{}, fmt.Errorf("exfat: invalid bytes-per-sector shift %d", bytesPerSectorShift)
	}
	sectorsPerClusterShift := buf[109]
	if sectorsPerClusterShift > 25-bytesPerSectorShift {
		return Info{}, fmt.Errorf("exfat: invalid sectors-per-cluster shift %d", sectorsPerClusterShift)
	}
	numberOfFATs := buf[110]
	if numberOfFATs == 0 {
		return Info{}, fmt.Errorf("exfat: FAT count is zero")
	}

	return Info{
		PartitionStartSector:   le.Uint64(buf[64:]),
		VolumeLength:           volumeLength,
		FATOffset:              fatOffset,
		FATLength:              fatLength,
		ClusterHeapOffset:      clusterHeapOffset,
		ClusterCount:           clusterCount,
		RootDirectoryCluster:   rootDirectoryCluster,
		VolumeSerialNumber:     le.Uint32(buf[100:]),
		FileSystemRevision:     le.Uint16(buf[104:]),
		VolumeFlags:            le.Uint16(buf[106:]),
		BytesPerSectorShift:    bytesPerSectorShift,
		SectorsPerClusterShift: sectorsPerClusterShift,
		NumberOfFATs:           numberOfFATs,
		DriveSelect:            buf[111],
		PercentInUse:           buf[112],
	}, nil
}

func partitionOffset(r io.ReaderAt, partIndex int) (int64, error) {
	var sig [8]byte
	if _, err := r.ReadAt(sig[:], sectorSize); err == nil && string(sig[:]) == "EFI PART" {
		return gptPartOffset(r, partIndex)
	}

	var magic [2]byte
	if _, err := r.ReadAt(magic[:], 510); err == nil && magic[0] == 0x55 && magic[1] == 0xAA {
		return mbrPartOffset(r, partIndex)
	}

	return 0, nil
}

func gptPartOffset(r io.ReaderAt, partIndex int) (int64, error) {
	hdr := make([]byte, 92)
	if _, err := r.ReadAt(hdr, sectorSize); err != nil {
		return 0, fmt.Errorf("exfat: read GPT header: %w", err)
	}
	le := binary.LittleEndian
	partEntryLBA := le.Uint64(hdr[72:])
	numParts := le.Uint32(hdr[80:])
	entrySize := le.Uint32(hdr[84:])
	if entrySize < 128 {
		return 0, fmt.Errorf("exfat: unexpected GPT entry size %d", entrySize)
	}

	tableOff := int64(partEntryLBA) * sectorSize
	buf := make([]byte, entrySize)
	for index := uint32(0); index < numParts; index++ {
		if _, err := r.ReadAt(buf, tableOff+int64(index)*int64(entrySize)); err != nil {
			break
		}
		var typeGUID [16]byte
		copy(typeGUID[:], buf[:16])
		startLBA := le.Uint64(buf[32:])

		if partIndex >= 0 {
			if int(index) != partIndex {
				continue
			}
			if typeGUID == [16]byte{} || startLBA == 0 {
				return 0, fmt.Errorf("exfat: GPT partition index %d not found", partIndex)
			}
			return int64(startLBA) * sectorSize, nil
		}

		if typeGUID != [16]byte{} && startLBA != 0 {
			return int64(startLBA) * sectorSize, nil
		}
	}

	if partIndex >= 0 {
		return 0, fmt.Errorf("exfat: GPT partition index %d not found", partIndex)
	}
	return 0, fmt.Errorf("exfat: no populated GPT partition found")
}

func mbrPartOffset(r io.ReaderAt, partIndex int) (int64, error) {
	table := make([]byte, 64)
	if _, err := r.ReadAt(table, 446); err != nil {
		return 0, fmt.Errorf("exfat: read MBR partition table: %w", err)
	}
	for index := 0; index < 4; index++ {
		entry := table[index*16:]
		startLBA := binary.LittleEndian.Uint32(entry[8:])

		if partIndex >= 0 {
			if index != partIndex {
				continue
			}
			if startLBA == 0 {
				return 0, fmt.Errorf("exfat: MBR partition index %d not found", partIndex)
			}
			return int64(startLBA) * sectorSize, nil
		}

		if startLBA != 0 {
			return int64(startLBA) * sectorSize, nil
		}
	}

	if partIndex >= 0 {
		return 0, fmt.Errorf("exfat: MBR partition index %d not found", partIndex)
	}
	return 0, nil
}

func parseRootDirEntries(buf []byte) ([]filesystem.DirEntry, error) {
	metadata, err := parseRootDirMetadata(buf)
	if err != nil {
		return nil, err
	}
	entries := make([]filesystem.DirEntry, 0, len(metadata))
	for _, entry := range metadata {
		entries = append(entries, filesystem.NewDirEntry(uint64(entry.cluster), entry.name, uint8(entry.attr)))
	}
	return entries, nil
}

func parseRootDirMetadata(buf []byte) ([]rootDirEntry, error) {
	entries := make([]rootDirEntry, 0)
	le := binary.LittleEndian
	for offset := 0; offset+dirEntrySize <= len(buf); offset += dirEntrySize {
		entry := buf[offset : offset+dirEntrySize]
		typ := entry[0]
		if typ == exfatEntryEnd {
			return entries, nil
		}
		if typ != exfatEntryFile {
			continue
		}
		secondaryCount := int(entry[1])
		if secondaryCount < 2 || offset+(secondaryCount+1)*dirEntrySize > len(buf) {
			return nil, fmt.Errorf("exfat: truncated file entry set")
		}
		stream := buf[offset+dirEntrySize : offset+2*dirEntrySize]
		if stream[0] != exfatEntryStream {
			return nil, fmt.Errorf("exfat: file entry missing stream extension")
		}
		nameLen := int(stream[3])
		nameWords := make([]uint16, 0, secondaryCount*nameCharsPerEntry)
		for index := 2; index <= secondaryCount; index++ {
			nameEntry := buf[offset+index*dirEntrySize : offset+(index+1)*dirEntrySize]
			if nameEntry[0] != exfatEntryName {
				return nil, fmt.Errorf("exfat: file entry missing filename entry")
			}
			for charOffset := 2; charOffset < dirEntrySize; charOffset += 2 {
				nameWords = append(nameWords, le.Uint16(nameEntry[charOffset:charOffset+2]))
			}
		}
		if nameLen > len(nameWords) {
			return nil, fmt.Errorf("exfat: filename length exceeds entry set")
		}
		attrs := le.Uint16(entry[4:6])
		firstCluster := le.Uint32(stream[20:24])
		dataLength := le.Uint64(stream[24:32])
		name := string(utf16.Decode(nameWords[:nameLen]))
		entries = append(entries, rootDirEntry{name: name, attr: attrs, cluster: firstCluster, size: dataLength})
		offset += secondaryCount * dirEntrySize
	}
	return entries, nil
}

func (entry rootDirEntry) mode() uint16 {
	if entry.attr&exfatAttrDir != 0 {
		if entry.attr&exfatAttrReadOnly != 0 {
			return exfatModeDirRO
		}
		return exfatModeDir
	}
	if entry.attr&exfatAttrReadOnly != 0 {
		return exfatModeFileRO
	}
	return exfatModeFile
}

func rootPathName(path string, prefix string) (string, error) {
	if !strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("%s: unsupported path %q", prefix, path)
	}
	name := strings.TrimPrefix(path, "/")
	if name == "" {
		return "", nil
	}
	if strings.Contains(name, "/") {
		return "", fmt.Errorf("%s: nested paths are not supported %q", prefix, path)
	}
	return name, nil
}

// pathComponents splits a path like "/a/b/c" into ["a", "b", "c"].
func pathComponents(path string) []string {
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return nil
	}
	return strings.Split(path, "/")
}

// readDirBuf reads the full cluster chain belonging to the directory at startCluster.
func (fs *exfatFS) readDirBuf(startCluster uint32) ([]byte, error) {
	const maxDirClusters = 256
	maxBytes := uint64(maxDirClusters) * fs.info.ClusterSize()
	return fs.readClusterChain(startCluster, maxBytes)
}

// writeDirBuf writes buf back to the cluster chain starting at startCluster.
func (fs *exfatFS) writeDirBuf(startCluster uint32, buf []byte) error {
	clusterSize := int64(fs.info.ClusterSize())
	dataBase := fs.info.ClusterHeapOffsetBytes(fs.partOffset)
	fatBase := fs.info.FATOffsetBytes(fs.partOffset)
	cluster := startCluster
	for pos := 0; pos < len(buf); pos += int(clusterSize) {
		off := dataBase + int64(cluster-2)*clusterSize
		end := pos + int(clusterSize)
		if end > len(buf) {
			end = len(buf)
		}
		padded := make([]byte, clusterSize)
		copy(padded, buf[pos:end])
		if _, err := fs.f.WriteAt(padded, off); err != nil {
			return fmt.Errorf("exfat: write directory cluster %d: %w", cluster, err)
		}
		if end >= len(buf) {
			break
		}
		var next [4]byte
		if _, err := fs.f.ReadAt(next[:], fatBase+int64(cluster)*4); err != nil {
			return fmt.Errorf("exfat: read FAT entry for cluster %d: %w", cluster, err)
		}
		cluster = binary.LittleEndian.Uint32(next[:])
	}
	return nil
}

// resolvePath resolves an absolute path and returns (entry, parentCluster, error).
// For "/" it returns a synthesised root entry.
func (fs *exfatFS) resolvePath(path string) (rootDirEntry, uint32, error) {
	if !strings.HasPrefix(path, "/") {
		return rootDirEntry{}, 0, fmt.Errorf("exfat: unsupported path %q", path)
	}
	parts := pathComponents(path)
	if len(parts) == 0 {
		return rootDirEntry{cluster: fs.info.RootDirectoryCluster, attr: exfatAttrDir, size: fs.info.ClusterSize()}, 0, nil
	}
	curCluster := fs.info.RootDirectoryCluster
	var result rootDirEntry
	var parentCluster uint32
	for i, name := range parts {
		buf, err := fs.readDirBuf(curCluster)
		if err != nil {
			return rootDirEntry{}, 0, err
		}
		entries, err := parseRootDirMetadata(buf)
		if err != nil {
			return rootDirEntry{}, 0, err
		}
		found := false
		for _, e := range entries {
			if strings.EqualFold(e.name, name) {
				if i < len(parts)-1 && e.attr&exfatAttrDir == 0 {
					return rootDirEntry{}, 0, fmt.Errorf("exfat: %q is not a directory", name)
				}
				parentCluster = curCluster
				result = e
				curCluster = e.cluster
				found = true
				break
			}
		}
		if !found {
			return rootDirEntry{}, 0, fmt.Errorf("exfat: %q not found", path)
		}
	}
	return result, parentCluster, nil
}

// getParentDir returns the name of the last path component and the cluster of its parent directory.
func (fs *exfatFS) getParentDir(path string) (name string, parentCluster uint32, err error) {
	parts := pathComponents(path)
	if len(parts) == 0 {
		return "", 0, fmt.Errorf("exfat: invalid path %q", path)
	}
	if !strings.HasPrefix(path, "/") {
		return "", 0, fmt.Errorf("exfat: unsupported path %q", path)
	}
	name = parts[len(parts)-1]
	if len(parts) == 1 {
		return name, fs.info.RootDirectoryCluster, nil
	}
	parent, _, err := fs.resolvePath("/" + strings.Join(parts[:len(parts)-1], "/"))
	if err != nil {
		return "", 0, err
	}
	if parent.attr&exfatAttrDir == 0 {
		return "", 0, fmt.Errorf("exfat: parent of %q is not a directory", path)
	}
	return name, parent.cluster, nil
}

// readClusterChain follows the FAT chain starting at start and returns up to size bytes.
func (fs *exfatFS) readClusterChain(start uint32, size uint64) ([]byte, error) {
	if start == 0 {
		return []byte{}, nil
	}
	clusterSize := int64(fs.info.ClusterSize())
	fatBase := fs.info.FATOffsetBytes(fs.partOffset)
	dataBase := fs.info.ClusterHeapOffsetBytes(fs.partOffset)
	buf := make([]byte, 0, size)
	cluster := start
	for {
		if cluster < 2 || cluster >= 0xFFFFFFF7 {
			break
		}
		clusterBuf := make([]byte, clusterSize)
		off := dataBase + int64(cluster-2)*clusterSize
		if _, err := fs.f.ReadAt(clusterBuf, off); err != nil {
			return nil, fmt.Errorf("exfat: read cluster %d: %w", cluster, err)
		}
		buf = append(buf, clusterBuf...)
		var nextEntry [4]byte
		if _, err := fs.f.ReadAt(nextEntry[:], fatBase+int64(cluster)*4); err != nil {
			return nil, fmt.Errorf("exfat: read FAT entry for cluster %d: %w", cluster, err)
		}
		next := binary.LittleEndian.Uint32(nextEntry[:])
		if next >= 0xFFFFFFF8 {
			break
		}
		cluster = next
	}
	if uint64(len(buf)) > size {
		buf = buf[:size]
	}
	return buf, nil
}

// writeData allocates FAT clusters, writes data into them, and returns the first cluster.
func (fs *exfatFS) writeData(data []byte) (uint32, error) {
	clusterSize := int64(fs.info.ClusterSize())
	numClusters := (int64(len(data)) + clusterSize - 1) / clusterSize
	allocated := make([]uint32, numClusters)
	for i := range allocated {
		c, err := fs.allocCluster()
		if err != nil {
			for _, ac := range allocated[:i] {
				_ = fs.setFATEntry(ac, 0)
			}
			return 0, err
		}
		if err := fs.setFATEntry(c, 0xFFFFFFFF); err != nil {
			return 0, err
		}
		allocated[i] = c
	}
	for i := 0; i < len(allocated)-1; i++ {
		if err := fs.setFATEntry(allocated[i], allocated[i+1]); err != nil {
			return 0, err
		}
	}
	dataBase := fs.info.ClusterHeapOffsetBytes(fs.partOffset)
	for i, c := range allocated {
		off := dataBase + int64(c-2)*clusterSize
		start := int64(i) * clusterSize
		end := start + clusterSize
		if end > int64(len(data)) {
			clusterBuf := make([]byte, clusterSize)
			copy(clusterBuf, data[start:])
			if _, err := fs.f.WriteAt(clusterBuf, off); err != nil {
				return 0, fmt.Errorf("exfat: write cluster %d: %w", c, err)
			}
		} else {
			if _, err := fs.f.WriteAt(data[start:end], off); err != nil {
				return 0, fmt.Errorf("exfat: write cluster %d: %w", c, err)
			}
		}
	}
	return allocated[0], nil
}

// allocCluster scans the FAT and returns the first free cluster number (≥ 2).
func (fs *exfatFS) allocCluster() (uint32, error) {
	fatBase := fs.info.FATOffsetBytes(fs.partOffset)
	var buf [4]byte
	for c := uint32(2); c < fs.info.ClusterCount+2; c++ {
		if _, err := fs.f.ReadAt(buf[:], fatBase+int64(c)*4); err != nil {
			return 0, fmt.Errorf("exfat: read FAT entry: %w", err)
		}
		if binary.LittleEndian.Uint32(buf[:]) == 0 {
			return c, nil
		}
	}
	return 0, fmt.Errorf("exfat: no free clusters")
}

// setFATEntry writes a 32-bit FAT entry for cluster.
func (fs *exfatFS) setFATEntry(cluster uint32, value uint32) error {
	fatBase := fs.info.FATOffsetBytes(fs.partOffset)
	off := fatBase + int64(cluster)*4
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], value)
	if _, err := fs.f.WriteAt(buf[:], off); err != nil {
		return fmt.Errorf("exfat: write FAT entry for cluster %d: %w", cluster, err)
	}
	return nil
}

// freeChain marks every cluster in the FAT chain starting at start as free.
func (fs *exfatFS) freeChain(start uint32) error {
	fatBase := fs.info.FATOffsetBytes(fs.partOffset)
	cluster := start
	for cluster >= 2 && cluster < 0xFFFFFFF7 {
		var next [4]byte
		if _, err := fs.f.ReadAt(next[:], fatBase+int64(cluster)*4); err != nil {
			return fmt.Errorf("exfat: read FAT entry for cluster %d: %w", cluster, err)
		}
		nextCluster := binary.LittleEndian.Uint32(next[:])
		if err := fs.setFATEntry(cluster, 0); err != nil {
			return err
		}
		if nextCluster >= 0xFFFFFFF8 {
			break
		}
		cluster = nextCluster
	}
	return nil
}

// writeRootDir writes the root directory cluster buffer back to disk.
func (fs *exfatFS) writeRootDir(buf []byte) error {
	return fs.writeDirBuf(fs.info.RootDirectoryCluster, buf)
}

// makeExFATEntrySet creates a checksummed exFAT directory entry set.
func makeExFATEntrySet(name string, attrs uint16, cluster uint32, size uint64) []byte {
	nameWords := utf16.Encode([]rune(name))
	numNameEntries := (len(nameWords) + nameCharsPerEntry - 1) / nameCharsPerEntry
	secondaryCount := uint8(1 + numNameEntries)
	buf := make([]byte, int(1+secondaryCount)*dirEntrySize)
	le := binary.LittleEndian

	buf[0] = exfatEntryFile
	buf[1] = secondaryCount
	le.PutUint16(buf[4:6], attrs)

	s := buf[dirEntrySize:]
	s[0] = exfatEntryStream
	s[1] = 1 // allocationPossible
	s[3] = uint8(len(nameWords))
	le.PutUint16(s[4:6], exfatNameHash(name))
	le.PutUint64(s[8:16], size)
	le.PutUint32(s[20:24], cluster)
	le.PutUint64(s[24:32], size)

	for i := 0; i < numNameEntries; i++ {
		n := buf[(2+i)*dirEntrySize:]
		n[0] = exfatEntryName
		start := i * nameCharsPerEntry
		end := start + nameCharsPerEntry
		if end > len(nameWords) {
			end = len(nameWords)
		}
		for j, w := range nameWords[start:end] {
			le.PutUint16(n[2+j*2:], w)
		}
	}

	le.PutUint16(buf[2:4], exfatEntrySetChecksum(buf))
	return buf
}

// exfatNameHash computes the exFAT name hash for an uppercase UTF-16LE name.
func exfatNameHash(name string) uint16 {
	var hash uint16
	for _, w := range utf16.Encode([]rune(strings.ToUpper(name))) {
		hash = (hash >> 1) | (hash << 15)
		hash += uint16(w & 0xFF)
		hash = (hash >> 1) | (hash << 15)
		hash += uint16(w >> 8)
	}
	return hash
}

// exfatEntrySetChecksum computes the checksum over all entries in an entry set.
func exfatEntrySetChecksum(entries []byte) uint16 {
	var checksum uint16
	n := len(entries) / dirEntrySize
	for i := 0; i < n; i++ {
		for j := 0; j < dirEntrySize; j++ {
			if i == 0 && (j == 2 || j == 3) {
				continue
			}
			checksum = (checksum >> 1) | (checksum << 15)
			checksum += uint16(entries[i*dirEntrySize+j])
		}
	}
	return checksum
}

// exfatFindEntry searches for an in-use file entry with the given name (case-insensitive).
// Returns (offset, secondaryCount) if found, (-1, 0) otherwise.
func exfatFindEntry(buf []byte, name string) (int, int) {
	le := binary.LittleEndian
	for offset := 0; offset+dirEntrySize <= len(buf); {
		typ := buf[offset]
		if typ == exfatEntryEnd {
			break
		}
		if typ&0x80 == 0 {
			offset += dirEntrySize
			continue
		}
		if typ != exfatEntryFile {
			offset += dirEntrySize
			continue
		}
		secondaryCount := int(buf[offset+1])
		if offset+(secondaryCount+1)*dirEntrySize > len(buf) {
			break
		}
		stream := buf[offset+dirEntrySize : offset+2*dirEntrySize]
		nameLen := int(stream[3])
		nameWords := make([]uint16, 0, nameLen)
		for i := 2; i <= secondaryCount; i++ {
			ne := buf[offset+i*dirEntrySize : offset+(i+1)*dirEntrySize]
			if ne[0] != exfatEntryName {
				break
			}
			for j := 2; j < dirEntrySize; j += 2 {
				nameWords = append(nameWords, le.Uint16(ne[j:j+2]))
			}
		}
		if len(nameWords) >= nameLen {
			entryName := string(utf16.Decode(nameWords[:nameLen]))
			if strings.EqualFold(entryName, name) {
				return offset, secondaryCount
			}
		}
		offset += (secondaryCount + 1) * dirEntrySize
	}
	return -1, 0
}

// exfatFindFreeSlot returns the offset of the first position with at least setSize
// consecutive free (0x00) entries. Returns -1 if no slot is available.
func exfatFindFreeSlot(buf []byte, setSize int) int {
	for offset := 0; offset+dirEntrySize <= len(buf); {
		typ := buf[offset]
		if typ == exfatEntryEnd {
			remaining := (len(buf) - offset) / dirEntrySize
			if remaining >= setSize {
				return offset
			}
			return -1
		}
		if typ&0x80 == 0 {
			offset += dirEntrySize
			continue
		}
		if typ == exfatEntryFile {
			secondaryCount := int(buf[offset+1])
			if offset+(secondaryCount+1)*dirEntrySize > len(buf) {
				return -1
			}
			offset += (secondaryCount + 1) * dirEntrySize
		} else {
			offset += dirEntrySize
		}
	}
	return -1
}

// exfatDeleteEntry marks the entry set beginning at offset as deleted.
func exfatDeleteEntry(buf []byte, offset int, secondaryCount int) {
	for i := 0; i <= secondaryCount; i++ {
		off := offset + i*dirEntrySize
		if off+dirEntrySize <= len(buf) {
			buf[off] &^= 0x80
		}
	}
}
