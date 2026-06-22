package filesystem_exfat

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

type errorReaderAt struct {
	data       []byte
	failOffset int64
	size       int64 // reported by Size(); used by readerSize for the gpt parser
}

// Size lets partitionOffset's readerSize discover the device extent so the
// hardened go-volumes/gpt parser validates partition offsets against it.
func (reader errorReaderAt) Size() int64 { return reader.size }

func (reader errorReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off == reader.failOffset {
		return 0, io.ErrUnexpectedEOF
	}
	if off >= int64(len(reader.data)) {
		return 0, io.EOF
	}
	n := copy(p, reader.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// mockDisk is an in-memory disk that can inject read/write errors at specific offsets.
type mockDisk struct {
	data     []byte
	readErr  func(off int64) error
	writeErr func(off int64) error
}

func (m *mockDisk) ReadAt(p []byte, off int64) (int, error) {
	if m.readErr != nil {
		if err := m.readErr(off); err != nil {
			return 0, err
		}
	}
	if off >= int64(len(m.data)) {
		return 0, io.EOF
	}
	n := copy(p, m.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (m *mockDisk) WriteAt(p []byte, off int64) (int, error) {
	if m.writeErr != nil {
		if err := m.writeErr(off); err != nil {
			return 0, err
		}
	}
	need := int(off) + len(p)
	if need > len(m.data) {
		m.data = append(m.data, make([]byte, need-len(m.data))...)
	}
	copy(m.data[off:], p)
	return len(p), nil
}

func (m *mockDisk) Close() error { return nil }

func newMockFSWithErrors(data []byte, info Info, partOffset int64, readErr, writeErr func(int64) error) *exfatFS {
	return &exfatFS{f: &mockDisk{data: data, readErr: readErr, writeErr: writeErr}, partOffset: partOffset, info: info}
}

func TestOpenBareImageAndInfoHelpers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exfat.img")
	if err := os.WriteFile(path, defaultExFATBootSector(), 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	fs := openTestFS(t, path)

	info := fs.Info()
	if fs.PartitionOffset() != 0 {
		t.Fatalf("PartitionOffset() = %d, want 0", fs.PartitionOffset())
	}
	if got, want := info.BytesPerSector(), uint32(512); got != want {
		t.Fatalf("BytesPerSector() = %d, want %d", got, want)
	}
	if got, want := info.SectorsPerCluster(), uint32(8); got != want {
		t.Fatalf("SectorsPerCluster() = %d, want %d", got, want)
	}
	if got, want := info.ClusterSize(), uint64(4096); got != want {
		t.Fatalf("ClusterSize() = %d, want %d", got, want)
	}
	if got, want := info.FATOffsetBytes(0), int64(24*512); got != want {
		t.Fatalf("FATOffsetBytes() = %d, want %d", got, want)
	}
	if got, want := info.ClusterHeapOffsetBytes(0), int64(280*512); got != want {
		t.Fatalf("ClusterHeapOffsetBytes() = %d, want %d", got, want)
	}
	if got, want := info.RootDirOffset(0), int64(280*512); got != want {
		t.Fatalf("RootDirOffset() = %d, want %d", got, want)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestOpenWithMBRPartition(t *testing.T) {
	image := make([]byte, 4096*sectorSize)
	writeMBRPartition(image, 0, 1024)
	copy(image[1024*sectorSize:], defaultExFATBootSector())

	path := filepath.Join(t.TempDir(), "exfat-mbr.img")
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	fs := openTestFS(t, path)
	defer fs.Close()

	if got, want := fs.PartitionOffset(), int64(1024*sectorSize); got != want {
		t.Fatalf("PartitionOffset() = %d, want %d", got, want)
	}
}

func TestListDirRoot(t *testing.T) {
	image := make([]byte, 1024*1024)
	boot := defaultExFATBootSector()
	copy(image, boot)
	root := image[rootDirOffsetFromBoot(boot):]
	writeExFATEntrySet(root[0:], "README.TXT", 5, false)
	writeExFATEntrySet(root[96:], "SUBDIR", 7, true)
	root[192] = exfatEntryEnd

	path := filepath.Join(t.TempDir(), "exfat-list.img")
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	fs := openTestFS(t, path)
	defer fs.Close()

	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2", len(entries))
	}
	if entries[0].Name() != "README.TXT" || entries[0].Inode() != 5 || entries[0].FileType() != 0x20 {
		t.Fatalf("entries[0] = %+v, want README.TXT cluster 5 file", entries[0])
	}
	if entries[1].Name() != "SUBDIR" || entries[1].Inode() != 7 || entries[1].FileType() != 0x10 {
		t.Fatalf("entries[1] = %+v, want SUBDIR cluster 7 dir", entries[1])
	}
}

func TestStatRootAndEntries(t *testing.T) {
	image := make([]byte, 1024*1024)
	boot := defaultExFATBootSector()
	copy(image, boot)
	root := image[rootDirOffsetFromBoot(boot):]
	writeExFATEntrySetWithAttr(root[0:], "README.TXT", 5, exfatAttrReadOnly, 1234)
	writeExFATEntrySetWithAttr(root[96:], "SUBDIR", 7, exfatAttrDir, 0)
	root[192] = exfatEntryEnd

	path := filepath.Join(t.TempDir(), "exfat-stat.img")
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	fs := openTestFS(t, path)
	defer fs.Close()

	rootStat, err := fs.Stat("/")
	if err != nil {
		t.Fatalf("Stat(/): %v", err)
	}
	if rootStat.Mode() != exfatModeDir || rootStat.Size() != fs.info.ClusterSize() || rootStat.Inode() != uint64(fs.info.RootDirectoryCluster) {
		t.Fatalf("rootStat = (%o,%d,%d), want (%o,%d,%d)", rootStat.Mode(), rootStat.Size(), rootStat.Inode(), exfatModeDir, fs.info.ClusterSize(), fs.info.RootDirectoryCluster)
	}

	fileStat, err := fs.Stat("/readme.txt")
	if err != nil {
		t.Fatalf("Stat(/readme.txt): %v", err)
	}
	if fileStat.Mode() != exfatModeFileRO || fileStat.Size() != 1234 || fileStat.Inode() != 5 {
		t.Fatalf("fileStat = (%o,%d,%d), want (%o,1234,5)", fileStat.Mode(), fileStat.Size(), fileStat.Inode(), exfatModeFileRO)
	}

	dirStat, err := fs.Stat("/SUBDIR")
	if err != nil {
		t.Fatalf("Stat(/SUBDIR): %v", err)
	}
	if dirStat.Mode() != exfatModeDir || dirStat.Size() != 0 || dirStat.Inode() != 7 {
		t.Fatalf("dirStat = (%o,%d,%d), want (%o,0,7)", dirStat.Mode(), dirStat.Size(), dirStat.Inode(), exfatModeDir)
	}
}

func TestListDirErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exfat.img")
	boot := defaultExFATBootSector()
	if err := os.WriteFile(path, boot, 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	if _, err := fs.ListDir("/nested"); err == nil {
		t.Fatal("ListDir() error = nil, want unsupported path error")
	}
	if _, err := fs.ListDir("/"); err == nil {
		t.Fatal("ListDir() error = nil, want root read error on truncated image")
	}
}

func TestStatErrors(t *testing.T) {
	image := make([]byte, 1024*1024)
	boot := defaultExFATBootSector()
	copy(image, boot)
	root := image[rootDirOffsetFromBoot(boot):]
	writeExFATEntrySetWithAttr(root[0:], "README.TXT", 5, 0x20, 42)
	root[96] = exfatEntryEnd

	path := filepath.Join(t.TempDir(), "exfat-stat-errors.img")
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	if _, err := fs.Stat("README.TXT"); err == nil {
		t.Fatal("Stat() error = nil, want unsupported relative path error")
	}
	if _, err := fs.Stat("/nested/file"); err == nil {
		t.Fatal("Stat() error = nil, want nested path error")
	}
	if _, err := fs.Stat("/missing.txt"); err == nil {
		t.Fatal("Stat() error = nil, want not found error")
	}

	truncatedPath := filepath.Join(t.TempDir(), "exfat-truncated.img")
	if err := os.WriteFile(truncatedPath, boot, 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}
	truncated, err := Open(truncatedPath, -1)
	if err != nil {
		t.Fatalf("Open(truncated): %v", err)
	}
	defer truncated.Close()
	if _, err := truncated.Stat("/README.TXT"); err == nil {
		t.Fatal("Stat() error = nil, want root read error on truncated image")
	}
}

func TestParseRootDirEntries(t *testing.T) {
	buf := make([]byte, 8*32)
	writeExFATEntrySet(buf[0:], "README.TXT", 5, false)
	buf[96] = 0x83
	writeExFATEntrySet(buf[128:], "SUBDIR", 7, true)
	buf[224] = exfatEntryEnd

	entries, err := parseRootDirEntries(buf)
	if err != nil {
		t.Fatalf("parseRootDirEntries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2", len(entries))
	}
	if entries[0].Name() != "README.TXT" || entries[1].Name() != "SUBDIR" {
		t.Fatalf("names = %q, %q, want README.TXT and SUBDIR", entries[0].Name(), entries[1].Name())
	}

	noTerminator := make([]byte, 3*32)
	writeExFATEntrySet(noTerminator, "BOOT", 3, false)
	entries, err = parseRootDirEntries(noTerminator)
	if err != nil {
		t.Fatalf("parseRootDirEntries(no terminator): %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "BOOT" {
		t.Fatalf("parseRootDirEntries(no terminator) = %+v, want single BOOT entry", entries)
	}
}

func TestParseRootDirEntryErrors(t *testing.T) {
	tests := []struct {
		name string
		buf  []byte
	}{
		{name: "truncated entry set", buf: func() []byte {
			buf := make([]byte, 2*32)
			buf[0] = exfatEntryFile
			buf[1] = 2
			return buf
		}()},
		{name: "missing stream", buf: func() []byte {
			buf := make([]byte, 3*32)
			buf[0] = exfatEntryFile
			buf[1] = 2
			buf[32] = exfatEntryName
			return buf
		}()},
		{name: "missing filename", buf: func() []byte {
			buf := make([]byte, 3*32)
			buf[0] = exfatEntryFile
			buf[1] = 2
			buf[32] = exfatEntryStream
			buf[32+3] = 1
			return buf
		}()},
		{name: "name too long", buf: func() []byte {
			buf := make([]byte, 3*32)
			buf[0] = exfatEntryFile
			buf[1] = 2
			buf[32] = exfatEntryStream
			buf[32+3] = 20
			buf[64] = exfatEntryName
			return buf
		}()},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseRootDirEntries(test.buf); err == nil {
				t.Fatal("parseRootDirEntries() error = nil, want error")
			}
		})
	}
}

func TestRootHelpers(t *testing.T) {
	if got := (rootDirEntry{attr: exfatAttrDir | exfatAttrReadOnly}).mode(); got != exfatModeDirRO {
		t.Fatalf("mode(readonly dir) = %o, want %o", got, exfatModeDirRO)
	}
	if got := (rootDirEntry{attr: 0x20}).mode(); got != exfatModeFile {
		t.Fatalf("mode(normal file) = %o, want %o", got, exfatModeFile)
	}
	name, err := rootPathName("/", "exfat")
	if err != nil {
		t.Fatalf("rootPathName(/): %v", err)
	}
	if name != "" {
		t.Fatalf("rootPathName(/) = %q, want empty string", name)
	}
	if name, err := rootPathName("/hello", "exfat"); err != nil || name != "hello" {
		t.Fatalf("rootPathName(/hello) = %q, %v, want hello, nil", name, err)
	}
	if _, err := rootPathName("/a/b", "exfat"); err == nil {
		t.Fatal("rootPathName(/a/b) error = nil, want error")
	}
	if _, err := rootPathName("noabs", "exfat"); err == nil {
		t.Fatal("rootPathName(noabs) error = nil, want error")
	}
}

func TestStatMalformedRoot(t *testing.T) {
	image := make([]byte, 1024*1024)
	boot := defaultExFATBootSector()
	copy(image, boot)
	root := image[rootDirOffsetFromBoot(boot):]
	root[0] = exfatEntryFile
	root[1] = 2
	root[32] = exfatEntryStream
	root[32+3] = 1
	root[96] = exfatEntryEnd

	path := filepath.Join(t.TempDir(), "exfat-malformed-root.img")
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	if _, err := fs.Stat("/README.TXT"); err == nil {
		t.Fatal("Stat() error = nil, want malformed root entry error")
	}
}

func TestOpenErrorPaths(t *testing.T) {
	origOpenFile := openFile
	origOpenPartitionOffset := openPartitionOffset
	origOpenReadInfo := openReadInfo
	t.Cleanup(func() {
		openFile = origOpenFile
		openPartitionOffset = origOpenPartitionOffset
		openReadInfo = origOpenReadInfo
	})

	openFile = func(string, int, os.FileMode) (*os.File, error) {
		return nil, errors.New("boom")
	}
	if _, err := Open("missing.img", -1); err == nil {
		t.Fatal("Open() error = nil, want error")
	}

	path := filepath.Join(t.TempDir(), "exfat.img")
	if err := os.WriteFile(path, defaultExFATBootSector(), 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	openFile = origOpenFile
	openPartitionOffset = func(io.ReaderAt, int) (int64, error) {
		return 0, errors.New("partition")
	}
	if _, err := Open(path, -1); err == nil {
		t.Fatal("Open() error = nil, want partition error")
	}

	openPartitionOffset = origOpenPartitionOffset
	openReadInfo = func(io.ReaderAt, int64) (Info, error) {
		return Info{}, errors.New("read")
	}
	if _, err := Open(path, -1); err == nil {
		t.Fatal("Open() error = nil, want read error")
	}
}

func TestReadInfoValidationErrors(t *testing.T) {
	if _, err := readInfo(bytes.NewReader([]byte("short")), 0); err == nil {
		t.Fatal("readInfo() error = nil, want short-read error")
	}

	tests := []struct {
		name   string
		mutate func([]byte)
	}{
		{name: "bad signature", mutate: func(buf []byte) { buf[510] = 0 }},
		{name: "bad name", mutate: func(buf []byte) { copy(buf[3:11], []byte("NOTEXFAT")) }},
		{name: "zero volume length", mutate: func(buf []byte) { binary.LittleEndian.PutUint64(buf[72:], 0) }},
		{name: "zero fat offset", mutate: func(buf []byte) { binary.LittleEndian.PutUint32(buf[80:], 0) }},
		{name: "zero fat length", mutate: func(buf []byte) { binary.LittleEndian.PutUint32(buf[84:], 0) }},
		{name: "zero cluster heap", mutate: func(buf []byte) { binary.LittleEndian.PutUint32(buf[88:], 0) }},
		{name: "zero cluster count", mutate: func(buf []byte) { binary.LittleEndian.PutUint32(buf[92:], 0) }},
		{name: "bad root cluster", mutate: func(buf []byte) { binary.LittleEndian.PutUint32(buf[96:], 1) }},
		{name: "bad bps shift", mutate: func(buf []byte) { buf[108] = 8 }},
		{name: "bad spc shift", mutate: func(buf []byte) { buf[109] = 17 }},
		{name: "zero fat count", mutate: func(buf []byte) { buf[110] = 0 }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			buf := defaultExFATBootSector()
			test.mutate(buf)
			if _, err := readInfo(bytes.NewReader(buf), 0); err == nil {
				t.Fatalf("readInfo() error = nil, want error")
			}
		})
	}
}

func TestPartitionOffsetVariants(t *testing.T) {
	t.Run("bare image", func(t *testing.T) {
		off, err := partitionOffset(bytes.NewReader(make([]byte, sectorSize)), -1)
		if err != nil {
			t.Fatalf("partitionOffset: %v", err)
		}
		if off != 0 {
			t.Fatalf("partitionOffset() = %d, want 0", off)
		}
	})

	t.Run("gpt auto and index", func(t *testing.T) {
		// Device big enough that startLBA 40 and 48 fall inside it.
		image := make([]byte, 64*sectorSize)
		writeGPT(image, 2, []uint64{40, 48})
		if off, err := partitionOffset(bytes.NewReader(image), -1); err != nil || off != int64(40*sectorSize) {
			t.Fatalf("partitionOffset(auto) = (%d, %v), want (%d, nil)", off, err, 40*sectorSize)
		}
		if off, err := partitionOffset(bytes.NewReader(image), 1); err != nil || off != int64(48*sectorSize) {
			t.Fatalf("partitionOffset(index) = (%d, %v), want (%d, nil)", off, err, 48*sectorSize)
		}
	})

	t.Run("gpt errors", func(t *testing.T) {
		short := make([]byte, sectorSize+8)
		copy(short[sectorSize:], []byte("EFI PART"))
		if _, err := partitionOffset(bytes.NewReader(short), -1); err == nil {
			t.Fatal("partitionOffset() error = nil, want GPT header error")
		}

		badEntrySize := make([]byte, 64*sectorSize)
		writeGPTHeaderOnly(badEntrySize, 2, 64, 1)
		if _, err := partitionOffset(bytes.NewReader(badEntrySize), -1); err == nil {
			t.Fatal("partitionOffset() error = nil, want GPT entry-size error")
		}

		truncated := make([]byte, 64*sectorSize)
		writeGPTHeaderOnly(truncated, 2, 128, 1)
		// Size() reports the full image so the entry array passes the
		// device-extent check, but the read at the table fails. The hardened
		// parser stops at the unreadable entry and yields an empty table; an
		// explicit index request surfaces "not found" rather than panicking.
		if _, err := partitionOffset(errorReaderAt{data: truncated, failOffset: 2 * sectorSize, size: int64(len(truncated))}, 0); err == nil {
			t.Fatal("partitionOffset() error = nil, want truncated GPT table error")
		}

		empty := make([]byte, 64*sectorSize)
		writeGPTHeaderOnly(empty, 2, 128, 1)
		// Auto mode degrades an empty-but-present table to offset 0.
		if off, err := partitionOffset(bytes.NewReader(empty), -1); err != nil || off != 0 {
			t.Fatalf("partitionOffset(auto empty GPT) = (%d, %v), want (0, nil)", off, err)
		}
		if _, err := partitionOffset(bytes.NewReader(empty), 0); err == nil {
			t.Fatal("partitionOffset() error = nil, want missing GPT index error")
		}
		if _, err := partitionOffset(bytes.NewReader(empty), 3); err == nil {
			t.Fatal("partitionOffset() error = nil, want out-of-range GPT index error")
		}
	})

	t.Run("mbr auto and index", func(t *testing.T) {
		image := make([]byte, 4096*sectorSize)
		writeMBRPartition(image, 1, 2048)
		if off, err := partitionOffset(bytes.NewReader(image), -1); err != nil || off != int64(2048*sectorSize) {
			t.Fatalf("partitionOffset(auto) = (%d, %v), want (%d, nil)", off, err, 2048*sectorSize)
		}
		if off, err := partitionOffset(bytes.NewReader(image), 1); err != nil || off != int64(2048*sectorSize) {
			t.Fatalf("partitionOffset(index) = (%d, %v), want (%d, nil)", off, err, 2048*sectorSize)
		}
	})

	t.Run("mbr errors", func(t *testing.T) {
		image := make([]byte, sectorSize)
		image[510] = 0x55
		image[511] = 0xAA
		if _, err := partitionOffset(errorReaderAt{data: image, failOffset: 446, size: int64(len(image))}, -1); err == nil {
			t.Fatal("partitionOffset() error = nil, want MBR read error")
		}
		// Auto mode on an MBR with no populated entries degrades to offset 0.
		if off, err := partitionOffset(bytes.NewReader(image), -1); err != nil || off != 0 {
			t.Fatalf("partitionOffset() = (%d, %v), want (0, nil)", off, err)
		}
		if _, err := partitionOffset(bytes.NewReader(image), 0); err == nil {
			t.Fatal("partitionOffset() error = nil, want missing MBR index error")
		}
		if _, err := partitionOffset(bytes.NewReader(image), 5); err == nil {
			t.Fatal("partitionOffset() error = nil, want out-of-range MBR index error")
		}
	})
}

func defaultExFATBootSector() []byte {
	buf := make([]byte, sectorSize)
	copy(buf[3:11], []byte("EXFAT   "))
	binary.LittleEndian.PutUint64(buf[64:], 0)
	binary.LittleEndian.PutUint64(buf[72:], 131072)
	binary.LittleEndian.PutUint32(buf[80:], 24)
	binary.LittleEndian.PutUint32(buf[84:], 128)
	binary.LittleEndian.PutUint32(buf[88:], 280)
	binary.LittleEndian.PutUint32(buf[92:], 4096)
	binary.LittleEndian.PutUint32(buf[96:], 2)
	binary.LittleEndian.PutUint32(buf[100:], 0xCAFEBABE)
	binary.LittleEndian.PutUint16(buf[104:], 0x0100)
	binary.LittleEndian.PutUint16(buf[106:], 0x0001)
	buf[108] = 9
	buf[109] = 3
	buf[110] = 1
	buf[111] = 0x80
	buf[112] = 42
	buf[510] = 0x55
	buf[511] = 0xAA
	return buf
}

func writeMBRPartition(image []byte, index int, startLBA uint32) {
	image[510] = 0x55
	image[511] = 0xAA
	entry := image[446+index*16:]
	binary.LittleEndian.PutUint32(entry[8:], startLBA)
	// One-sector partition: a non-zero sector count keeps the slot non-empty
	// for the hardened go-volumes/gpt parser.
	binary.LittleEndian.PutUint32(entry[12:], 1)
	entry[4] = 0x07
}

func writeGPT(image []byte, entryLBA uint64, starts []uint64) {
	writeGPTHeaderOnly(image, entryLBA, 128, uint32(len(starts)))
	for index, start := range starts {
		entry := image[int(entryLBA)*sectorSize+index*128:]
		// Non-zero type GUID marks the slot populated; the hardened parser
		// skips all-zero-GUID entries.
		entry[0] = byte(index + 1)
		binary.LittleEndian.PutUint64(entry[32:], start)
		// endLBA == startLBA: a one-sector partition that fits the device.
		binary.LittleEndian.PutUint64(entry[40:], start)
	}
}

func writeGPTHeaderOnly(image []byte, entryLBA uint64, entrySize uint32, numParts uint32) {
	copy(image[sectorSize:], []byte("EFI PART"))
	binary.LittleEndian.PutUint64(image[sectorSize+72:], entryLBA)
	binary.LittleEndian.PutUint32(image[sectorSize+80:], numParts)
	binary.LittleEndian.PutUint32(image[sectorSize+84:], entrySize)
}

func rootDirOffsetFromBoot(boot []byte) int {
	info, err := readInfo(bytes.NewReader(boot), 0)
	if err != nil {
		panic(err)
	}
	return int(info.RootDirOffset(0))
}

func writeExFATEntrySet(buf []byte, name string, cluster uint32, dir bool) {
	attrs := uint16(0x20)
	if dir {
		attrs = exfatAttrDir
	}
	writeExFATEntrySetWithAttr(buf, name, cluster, attrs, 0)
}

func writeExFATEntrySetWithAttr(buf []byte, name string, cluster uint32, attrs uint16, size uint64) {
	buf[0] = exfatEntryFile
	buf[1] = 2
	binary.LittleEndian.PutUint16(buf[4:6], attrs)

	buf[32] = exfatEntryStream
	buf[32+3] = uint8(len(name))
	binary.LittleEndian.PutUint32(buf[32+20:32+24], cluster)
	binary.LittleEndian.PutUint64(buf[32+24:32+32], size)

	buf[64] = exfatEntryName
	for index, ch := range []rune(name) {
		binary.LittleEndian.PutUint16(buf[64+2+index*2:], uint16(ch))
	}
}

// exfatTestImage builds a 1 MiB exFAT image with a given root-dir setup and
// optionally FAT chain and cluster data.  The root cluster FAT entry is always
// pre-initialised to EOF so allocCluster never steals it.
func exfatTestImage(t *testing.T, setupRoot func(root []byte), fatEntries map[uint32]uint32, clusterData map[uint32][]byte) string {
	t.Helper()
	image := make([]byte, 1024*1024)
	boot := defaultExFATBootSector()
	copy(image, boot)
	info, err := readInfo(bytes.NewReader(boot), 0)
	if err != nil {
		t.Fatalf("readInfo: %v", err)
	}
	root := image[int(info.RootDirOffset(0)):]
	if setupRoot != nil {
		setupRoot(root)
	}
	fatBase := int(info.FATOffsetBytes(0))
	// Mark the root cluster as in-use so allocCluster skips it.
	binary.LittleEndian.PutUint32(image[fatBase+int(info.RootDirectoryCluster)*4:], 0xFFFFFFFF)
	for cluster, next := range fatEntries {
		binary.LittleEndian.PutUint32(image[fatBase+int(cluster)*4:], next)
	}
	dataBase := int(info.ClusterHeapOffsetBytes(0))
	cs := int(info.ClusterSize())
	for cluster, data := range clusterData {
		off := dataBase + int(cluster-2)*cs
		copy(image[off:], data)
	}
	path := filepath.Join(t.TempDir(), "exfat-rw.img")
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}
	return path
}
