package filesystem_exfat

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadFileClusterReadError(t *testing.T) {
	boot := defaultExFATBootSector()
	info, _ := readInfo(bytes.NewReader(boot), 0)
	imageSize := int(info.RootDirOffset(0)) + int(info.ClusterSize())
	image := make([]byte, imageSize)
	copy(image, boot)
	fatBase := int(info.FATOffsetBytes(0))
	binary.LittleEndian.PutUint32(image[fatBase+int(info.RootDirectoryCluster)*4:], 0xFFFFFFFF)
	binary.LittleEndian.PutUint32(image[fatBase+3*4:], 0xFFFFFFFF)
	root := image[int(info.RootDirOffset(0)):]
	writeExFATEntrySetWithAttr(root[0:], "data.txt", 3, 0x20, 5)
	root[96] = 0x00

	path := filepath.Join(t.TempDir(), "exfat-trunc2.img")
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()
	if _, err := fs.ReadFile("/data.txt"); err == nil {
		t.Fatal("ReadFile on truncated image error = nil, want cluster read error")
	}
}

// ---- New tests for 100% coverage ----

// buildExFATImageWithEntry returns a 1 MiB exFAT image with one file entry
// "file.txt" at cluster 3 (containing "hello"), plus the parsed Info.
func buildExFATImageWithEntry(t *testing.T) ([]byte, Info) {
	t.Helper()
	image := make([]byte, 1024*1024)
	boot := defaultExFATBootSector()
	copy(image, boot)
	info, _ := readInfo(bytes.NewReader(boot), 0)
	fatBase := int(info.FATOffsetBytes(0))
	binary.LittleEndian.PutUint32(image[fatBase+int(info.RootDirectoryCluster)*4:], 0xFFFFFFFF)
	binary.LittleEndian.PutUint32(image[fatBase+3*4:], 0xFFFFFFFF)
	root := image[int(info.RootDirOffset(0)):]
	writeExFATEntrySetWithAttr(root[0:], "file.txt", 3, 0x20, 5)
	root[96] = 0x00
	dataBase := int(info.ClusterHeapOffsetBytes(0))
	cs := int(info.ClusterSize())
	copy(image[dataBase+(3-2)*cs:], []byte("hello"))
	return image, info
}

// buildExFATFullRootDirImage creates an exFAT image whose root dir cluster is
// completely filled with in-use entries (no end marker and no free slots).
func buildExFATFullRootDirImage(t *testing.T) string {
	t.Helper()
	image := make([]byte, 1024*1024)
	boot := defaultExFATBootSector()
	copy(image, boot)
	info, _ := readInfo(bytes.NewReader(boot), 0)
	fatBase := int(info.FATOffsetBytes(0))
	binary.LittleEndian.PutUint32(image[fatBase+int(info.RootDirectoryCluster)*4:], 0xFFFFFFFF)
	root := image[int(info.RootDirOffset(0)):]
	// Each entry set uses 3 entries (96 bytes). ClusterSize=4096, so at most 42 sets fit.
	cs := int(info.ClusterSize())
	slotsUsed := 0
	for off := 0; off+96 <= cs; off += 96 {
		n := []rune{'A' + rune(slotsUsed%26), '0' + rune(slotsUsed/26%10)}
		writeExFATEntrySetWithAttr(root[off:], string(n), uint32(slotsUsed+10), 0x20, 0)
		slotsUsed++
	}
	// Fill any remaining 32-byte slots with non-0x00 in-use entries.
	remaining := cs - slotsUsed*96
	for off := slotsUsed * 96; off+32 <= cs && remaining > 0; off += 32 {
		root[off] = 0x81 // in-use, non-file
		remaining -= 32
	}
	path := filepath.Join(t.TempDir(), "exfat-fullroot.img")
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}
	return path
}

func TestWriteFileRootDirFull(t *testing.T) {
	path := buildExFATFullRootDirImage(t)
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()
	if err := fs.WriteFile("/zzznewfile.txt", []byte("x"), 0o644); err == nil {
		t.Fatal("WriteFile with full root dir error = nil, want error")
	}
}

func TestMkDirRootDirFull(t *testing.T) {
	path := buildExFATFullRootDirImage(t)
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()
	if err := fs.MkDir("/zzznewdir", 0o755); err == nil {
		t.Fatal("MkDir with full root dir error = nil, want error")
	}
}

func TestRenameRootDirFull(t *testing.T) {
	// Start with a file in the root, make it full except for the one entry,
	// then rename to a new name — after deleting the old entry, the root is
	// still full (all other entries are in-use bitmaps), triggering freeOff<0.
	image := make([]byte, 1024*1024)
	boot := defaultExFATBootSector()
	copy(image, boot)
	info, _ := readInfo(bytes.NewReader(boot), 0)
	fatBase := int(info.FATOffsetBytes(0))
	binary.LittleEndian.PutUint32(image[fatBase+int(info.RootDirectoryCluster)*4:], 0xFFFFFFFF)
	binary.LittleEndian.PutUint32(image[fatBase+3*4:], 0xFFFFFFFF)
	root := image[int(info.RootDirOffset(0)):]
	cs := int(info.ClusterSize())
	// Write the file at offset 0.
	writeExFATEntrySetWithAttr(root[0:], "old.txt", 3, 0x20, 5)
	// Fill the rest with non-file in-use entries (0x81) so no free slot exists.
	for off := 96; off+32 <= cs; off += 32 {
		root[off] = 0x81
	}
	path := filepath.Join(t.TempDir(), "exfat-rename-full.img")
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()
	if err := fs.Rename("/old.txt", "/new.txt"); err == nil {
		t.Fatal("Rename root dir full error = nil, want error")
	}
}

func TestReadClusterChainBadCluster(t *testing.T) {
	// FAT[3] = 1 (< 2) — exercises the cluster < 2 guard in readClusterChain.
	path := exfatTestImage(t, func(root []byte) {
		writeExFATEntrySetWithAttr(root[0:], "data.txt", 3, 0x20, 5)
		root[96] = 0x00
	}, map[uint32]uint32{3: 1}, map[uint32][]byte{3: []byte("hello")})
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()
	data, err := fs.ReadFile("/data.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("data = %q, want %q", data, "hello")
	}
}

func TestWriteDataPartialAllocCleanup(t *testing.T) {
	image := make([]byte, 1024*1024)
	boot := defaultExFATBootSector()
	copy(image, boot)
	info, _ := readInfo(bytes.NewReader(boot), 0)
	fatBase := int(info.FATOffsetBytes(0))
	clusterCount := info.ClusterCount
	// Mark every cluster as EOF except cluster 3.
	for c := uint32(0); c < clusterCount+4; c++ {
		off := fatBase + int(c)*4
		if off+4 > len(image) {
			break
		}
		val := uint32(0xFFFFFFFF)
		if c == 3 {
			val = 0
		}
		binary.LittleEndian.PutUint32(image[off:], val)
	}
	root := image[int(info.RootDirOffset(0)):]
	root[0] = 0x00

	path := filepath.Join(t.TempDir(), "exfat-onefreecluster.img")
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	bigData := make([]byte, int(info.ClusterSize())+1)
	if err := fs.WriteFile("/big.txt", bigData, 0o644); err == nil {
		t.Fatal("WriteFile partial alloc error = nil, want error")
	}
}

// ---- exfatFindEntry / exfatFindFreeSlot structural tests ----

func TestExfatFindEntryEdgeCases(t *testing.T) {
	t.Run("in-use non-file entry skipped", func(t *testing.T) {
		// 0xC0 (stream, in-use) appears first — must be skipped by the typ!=0x85 guard.
		buf := make([]byte, 3*dirEntrySize)
		buf[0] = exfatEntryStream // 0xC0, in-use, not 0x85
		buf[32] = exfatEntryEnd
		off, _ := exfatFindEntry(buf, "x")
		if off != -1 {
			t.Fatalf("expected -1, got %d", off)
		}
	})

	t.Run("truncated file entry set", func(t *testing.T) {
		// secondaryCount claims more entries than buf holds.
		buf := make([]byte, 2*dirEntrySize)
		buf[0] = exfatEntryFile // 0x85
		buf[1] = 5              // secondaryCount=5, but only 1 more entry available
		off, _ := exfatFindEntry(buf, "x")
		if off != -1 {
			t.Fatalf("expected -1, got %d", off)
		}
	})

	t.Run("secondary entry not name", func(t *testing.T) {
		// i=2 secondary is 0xC0 (stream) instead of 0xC1 (name).
		buf := make([]byte, 4*dirEntrySize)
		buf[0] = exfatEntryFile    // 0x85
		buf[1] = 2                 // secondaryCount=2
		buf[32] = exfatEntryStream // 0xC0
		buf[32+3] = 3              // nameLen=3
		buf[64] = exfatEntryStream // NOT exfatEntryName (0xC1)
		buf[96] = exfatEntryEnd
		off, _ := exfatFindEntry(buf, "abc")
		if off != -1 {
			t.Fatalf("expected -1, got %d", off)
		}
	})
}

func TestExfatFindFreeSlotEdgeCases(t *testing.T) {
	t.Run("end marker with insufficient space", func(t *testing.T) {
		// ExfatEntryEnd at offset 0, only 1 entry remaining but need 3.
		buf := make([]byte, 2*dirEntrySize)
		buf[0] = exfatEntryEnd
		off := exfatFindFreeSlot(buf, 3)
		if off != -1 {
			t.Fatalf("expected -1, got %d", off)
		}
	})

	t.Run("non-file in-use entry (else branch)", func(t *testing.T) {
		// 0x81 (in-use, not file) entries filling entire buf — no end marker.
		buf := make([]byte, 3*dirEntrySize)
		buf[0] = 0x81
		buf[32] = 0x82
		buf[64] = 0x83
		off := exfatFindFreeSlot(buf, 1)
		if off != -1 {
			t.Fatalf("expected -1, got %d", off)
		}
	})

	t.Run("file entry extends beyond buf", func(t *testing.T) {
		// File entry claims 5 secondaries but buf only holds 2 more entries.
		buf := make([]byte, 3*dirEntrySize)
		buf[0] = exfatEntryFile // 0x85 in-use
		buf[1] = 5              // secondaryCount=5; 0+(5+1)*32=192 > 3*32=96
		off := exfatFindFreeSlot(buf, 1)
		if off != -1 {
			t.Fatalf("expected -1, got %d", off)
		}
	})
}

// ---- Mock-injection tests ----

func TestSetFATEntryWriteError(t *testing.T) {
	image, info := buildExFATImageWithEntry(t)
	fatBase := info.FATOffsetBytes(0)
	fs := newMockFSWithErrors(image, info, 0, nil, func(off int64) error {
		if off == fatBase+3*4 {
			return errors.New("write error")
		}
		return nil
	})
	if err := fs.setFATEntry(3, 0); err == nil {
		t.Fatal("setFATEntry write error = nil, want error")
	}
}

func TestAllocClusterReadError(t *testing.T) {
	image, info := buildExFATImageWithEntry(t)
	fatBase := info.FATOffsetBytes(0)
	fs := newMockFSWithErrors(image, info, 0, func(off int64) error {
		if off == fatBase+2*4 {
			return errors.New("disk error")
		}
		return nil
	}, nil)
	if _, err := fs.allocCluster(); err == nil {
		t.Fatal("allocCluster read error = nil, want error")
	}
}

func TestFreeChainReadError(t *testing.T) {
	image, info := buildExFATImageWithEntry(t)
	fatBase := info.FATOffsetBytes(0)
	fs := newMockFSWithErrors(image, info, 0, func(off int64) error {
		if off == fatBase+3*4 {
			return errors.New("read error in freeChain")
		}
		return nil
	}, nil)
	if err := fs.freeChain(3); err == nil {
		t.Fatal("freeChain read error = nil, want error")
	}
}

func TestFreeChainSetFATError(t *testing.T) {
	image, info := buildExFATImageWithEntry(t)
	fatBase := info.FATOffsetBytes(0)
	fs := newMockFSWithErrors(image, info, 0, nil, func(off int64) error {
		if off == fatBase+3*4 {
			return errors.New("write error in setFATEntry via freeChain")
		}
		return nil
	})
	if err := fs.freeChain(3); err == nil {
		t.Fatal("freeChain via setFATEntry write error = nil, want error")
	}
}

func TestReadClusterChainFATReadError(t *testing.T) {
	image, info := buildExFATImageWithEntry(t)
	fatBase := info.FATOffsetBytes(0)
	dataBase := info.ClusterHeapOffsetBytes(0)
	cs := int64(info.ClusterSize())
	clusterDataOff := dataBase + (3-2)*cs
	readClusterDone := false
	fs := newMockFSWithErrors(image, info, 0, func(off int64) error {
		if off == clusterDataOff {
			readClusterDone = true
			return nil
		}
		if readClusterDone && off == fatBase+3*4 {
			return errors.New("FAT read error")
		}
		return nil
	}, nil)
	if _, err := fs.readClusterChain(3, 5); err == nil {
		t.Fatal("readClusterChain FAT read error = nil, want error")
	}
}

func TestWriteDataSetFATEOFError(t *testing.T) {
	image, info := buildExFATImageWithEntry(t)
	fatBase := info.FATOffsetBytes(0)
	writeCount := 0
	fs := newMockFSWithErrors(image, info, 0, nil, func(off int64) error {
		if off == fatBase+4*4 {
			writeCount++
			if writeCount == 1 {
				return errors.New("write EOF mark error")
			}
		}
		return nil
	})
	if _, err := fs.writeData([]byte("x")); err == nil {
		t.Fatal("writeData setFAT EOF error = nil, want error")
	}
}

func TestWriteDataLinkError(t *testing.T) {
	image, info := buildExFATImageWithEntry(t)
	fatBase := info.FATOffsetBytes(0)
	binary.LittleEndian.PutUint32(image[fatBase+4*4:], 0)
	binary.LittleEndian.PutUint32(image[fatBase+5*4:], 0)
	clusterSize := int64(info.ClusterSize())
	bigData := make([]byte, clusterSize+1)
	writes := map[int64]int{}
	fs := newMockFSWithErrors(image, info, 0, nil, func(off int64) error {
		writes[off]++
		if off == fatBase+4*4 && writes[off] == 2 {
			return errors.New("link write error")
		}
		return nil
	})
	if _, err := fs.writeData(bigData); err == nil {
		t.Fatal("writeData link error = nil, want error")
	}
}

func TestWriteDataWriteClusterPaddedError(t *testing.T) {
	image, info := buildExFATImageWithEntry(t)
	fatBase := info.FATOffsetBytes(0)
	binary.LittleEndian.PutUint32(image[fatBase+4*4:], 0)
	clusterSize := int64(info.ClusterSize())
	dataBase := info.ClusterHeapOffsetBytes(0)
	clusterOff := dataBase + (4-2)*clusterSize
	fs := newMockFSWithErrors(image, info, 0, nil, func(off int64) error {
		if off == clusterOff {
			return errors.New("padded cluster write error")
		}
		return nil
	})
	if _, err := fs.writeData([]byte("x")); err == nil {
		t.Fatal("writeData padded write error = nil, want error")
	}
}

func TestWriteDataWriteClusterExactError(t *testing.T) {
	image, info := buildExFATImageWithEntry(t)
	fatBase := info.FATOffsetBytes(0)
	binary.LittleEndian.PutUint32(image[fatBase+4*4:], 0)
	clusterSize := int64(info.ClusterSize())
	dataBase := info.ClusterHeapOffsetBytes(0)
	clusterOff := dataBase + (4-2)*clusterSize
	fs := newMockFSWithErrors(image, info, 0, nil, func(off int64) error {
		if off == clusterOff {
			return errors.New("exact cluster write error")
		}
		return nil
	})
	exactData := make([]byte, clusterSize)
	if _, err := fs.writeData(exactData); err == nil {
		t.Fatal("writeData exact write error = nil, want error")
	}
}

func TestWriteRootDirError(t *testing.T) {
	image, info := buildExFATImageWithEntry(t)
	rootOff := info.RootDirOffset(0)
	fs := newMockFSWithErrors(image, info, 0, nil, func(off int64) error {
		if off == rootOff {
			return errors.New("root dir write error")
		}
		return nil
	})
	buf := make([]byte, info.ClusterSize())
	if err := fs.writeRootDir(buf); err == nil {
		t.Fatal("writeRootDir write error = nil, want error")
	}
}

func TestWriteFileFreeChainError(t *testing.T) {
	image, info := buildExFATImageWithEntry(t)
	fatBase := info.FATOffsetBytes(0)
	fs := newMockFSWithErrors(image, info, 0, nil, func(off int64) error {
		if off == fatBase+3*4 {
			return errors.New("freeChain write error")
		}
		return nil
	})
	if err := fs.WriteFile("/file.txt", []byte("new"), 0o644); err == nil {
		t.Fatal("WriteFile freeChain error = nil, want error")
	}
}

func TestDeleteFileFreeChainError(t *testing.T) {
	image, info := buildExFATImageWithEntry(t)
	fatBase := info.FATOffsetBytes(0)
	fs := newMockFSWithErrors(image, info, 0, nil, func(off int64) error {
		if off == fatBase+3*4 {
			return errors.New("freeChain write error")
		}
		return nil
	})
	if err := fs.DeleteFile("/file.txt"); err == nil {
		t.Fatal("DeleteFile freeChain error = nil, want error")
	}
}

func TestDeleteDirFreeChainError(t *testing.T) {
	image := make([]byte, 1024*1024)
	boot := defaultExFATBootSector()
	copy(image, boot)
	info, _ := readInfo(bytes.NewReader(boot), 0)
	fatBase := int(info.FATOffsetBytes(0))
	binary.LittleEndian.PutUint32(image[fatBase+int(info.RootDirectoryCluster)*4:], 0xFFFFFFFF)
	binary.LittleEndian.PutUint32(image[fatBase+3*4:], 0xFFFFFFFF)
	root := image[int(info.RootDirOffset(0)):]
	writeExFATEntrySetWithAttr(root[0:], "emptydir", 3, exfatAttrDir, 0)
	root[96] = 0x00
	// Empty dir cluster—no in-use file entries.
	dataBase := int(info.ClusterHeapOffsetBytes(0))
	cs := int(info.ClusterSize())
	dirBuf := image[dataBase+(3-2)*cs:]
	dirBuf[0] = 0x00

	fs := newMockFSWithErrors(image, info, 0, nil, func(off int64) error {
		if off == int64(fatBase)+3*4 {
			return errors.New("freeChain write error")
		}
		return nil
	})
	if err := fs.DeleteDir("/emptydir"); err == nil {
		t.Fatal("DeleteDir freeChain error = nil, want error")
	}
}

func TestMkDirSetFATError(t *testing.T) {
	image, info := buildExFATImageWithEntry(t)
	fatBase := info.FATOffsetBytes(0)
	binary.LittleEndian.PutUint32(image[fatBase+4*4:], 0)
	writeCount := 0
	fs := newMockFSWithErrors(image, info, 0, nil, func(off int64) error {
		if off == fatBase+4*4 {
			writeCount++
			if writeCount == 1 {
				return errors.New("setFATEntry write error")
			}
		}
		return nil
	})
	if err := fs.MkDir("/newdir", 0o755); err == nil {
		t.Fatal("MkDir setFATEntry error = nil, want error")
	}
}

func TestMkDirWriteClusterError(t *testing.T) {
	image, info := buildExFATImageWithEntry(t)
	fatBase := info.FATOffsetBytes(0)
	binary.LittleEndian.PutUint32(image[fatBase+4*4:], 0)
	clusterSize := int64(info.ClusterSize())
	dataBase := info.ClusterHeapOffsetBytes(0)
	clusterOff := dataBase + (4-2)*clusterSize
	fs := newMockFSWithErrors(image, info, 0, nil, func(off int64) error {
		if off == clusterOff {
			return errors.New("cluster write error")
		}
		return nil
	})
	if err := fs.MkDir("/newdir", 0o755); err == nil {
		t.Fatal("MkDir WriteAt cluster error = nil, want error")
	}
}

func TestRenameFreeChainError(t *testing.T) {
	image := make([]byte, 1024*1024)
	boot := defaultExFATBootSector()
	copy(image, boot)
	info, _ := readInfo(bytes.NewReader(boot), 0)
	fatBase := int(info.FATOffsetBytes(0))
	binary.LittleEndian.PutUint32(image[fatBase+int(info.RootDirectoryCluster)*4:], 0xFFFFFFFF)
	binary.LittleEndian.PutUint32(image[fatBase+3*4:], 0xFFFFFFFF)
	binary.LittleEndian.PutUint32(image[fatBase+4*4:], 0xFFFFFFFF)
	root := image[int(info.RootDirOffset(0)):]
	writeExFATEntrySetWithAttr(root[0:], "old.txt", 3, 0x20, 5)
	writeExFATEntrySetWithAttr(root[96:], "new.txt", 4, 0x20, 3)
	root[192] = 0x00

	fs := newMockFSWithErrors(image, info, 0, nil, func(off int64) error {
		if off == int64(fatBase)+4*4 {
			return errors.New("freeChain write error for target")
		}
		return nil
	})
	if err := fs.Rename("/old.txt", "/new.txt"); err == nil {
		t.Fatal("Rename freeChain error = nil, want error")
	}
}

func TestDeleteDirHasInactiveEntry(t *testing.T) {
	// Dir cluster has an inactive (low-bit-clear) entry — exercises the
	// "typ & 0x80 == 0" path in DeleteDir's empty-check loop.
	emptyCluster := make([]byte, 4096)
	// Set an inactive entry — high bit not set, so it's not in-use.
	emptyCluster[0] = 0x05 // not in-use (bit 7 = 0)
	emptyCluster[32] = 0x00

	path := exfatTestImage(t, func(root []byte) {
		writeExFATEntrySetWithAttr(root[0:], "mydir", 3, exfatAttrDir, 0)
		root[96] = 0x00
	}, map[uint32]uint32{3: 0xFFFFFFFF}, map[uint32][]byte{3: emptyCluster})

	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()
	if err := fs.DeleteDir("/mydir"); err != nil {
		t.Fatalf("DeleteDir(dir with inactive entry): %v", err)
	}
}

func TestDeleteDirHasInUseNonFileEntry(t *testing.T) {
	// Dir cluster has an in-use non-file entry (0x81 bitmap) — exercises the
	// "offset += dirEntrySize" path after the t==exfatEntryFile check.
	dirCluster := make([]byte, 4096)
	dirCluster[0] = 0x81 // in-use (bit 7 = 1), but not exfatEntryFile (0x85)
	dirCluster[32] = 0x00

	path := exfatTestImage(t, func(root []byte) {
		writeExFATEntrySetWithAttr(root[0:], "mydir2", 3, exfatAttrDir, 0)
		root[96] = 0x00
	}, map[uint32]uint32{3: 0xFFFFFFFF}, map[uint32][]byte{3: dirCluster})

	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()
	if err := fs.DeleteDir("/mydir2"); err != nil {
		t.Fatalf("DeleteDir(dir with in-use non-file entry): %v", err)
	}
}
func TestWriteDirBufFATReadError(t *testing.T) {
	image := make([]byte, 1024*1024)
	boot := defaultExFATBootSector()
	copy(image, boot)
	info, _ := readInfo(bytes.NewReader(boot), 0)
	fatBase := info.FATOffsetBytes(0)
	rootCluster := info.RootDirectoryCluster
	// Pass a 2-cluster buffer to force writeDirBuf to read FAT for the next cluster.
	buf := make([]byte, int(info.ClusterSize())*2)
	// Inject a read error when writeDirBuf tries to read the FAT entry for rootCluster.
	fs := newMockFSWithErrors(image, info, 0, func(off int64) error {
		if off == fatBase+int64(rootCluster)*4 {
			return errors.New("injected FAT read error")
		}
		return nil
	}, nil)
	if err := fs.writeDirBuf(rootCluster, buf); err == nil {
		t.Fatal("writeDirBuf FAT read error = nil, want error")
	}
}

func TestWriteDirBufMultiCluster(t *testing.T) {
	image := make([]byte, 1024*1024)
	boot := defaultExFATBootSector()
	copy(image, boot)
	info, _ := readInfo(bytes.NewReader(boot), 0)
	fatBase := int(info.FATOffsetBytes(0))
	cs := int(info.ClusterSize())
	rootCluster := info.RootDirectoryCluster
	nextCluster := rootCluster + 1
	// Build a FAT chain: rootCluster → nextCluster → EOC.
	binary.LittleEndian.PutUint32(image[fatBase+int(rootCluster)*4:], nextCluster)
	binary.LittleEndian.PutUint32(image[fatBase+int(nextCluster)*4:], 0xFFFFFFFF)
	fs := newMockFSWithErrors(image, info, 0, nil, nil)
	// Write a 2-cluster buffer; the second cluster should be written successfully.
	buf := make([]byte, cs*2)
	buf[cs] = exfatEntryFile // sentinel in 2nd cluster
	if err := fs.writeDirBuf(rootCluster, buf); err != nil {
		t.Fatalf("writeDirBuf multi-cluster: %v", err)
	}
}

func TestRenameCrossDirReadError(t *testing.T) {
	image, info := buildExFATImageWithEntry(t)
	fatBase := int(info.FATOffsetBytes(0))
	cs := int(info.ClusterSize())
	dataBase := int(info.ClusterHeapOffsetBytes(0))
	// Add a subdirectory at cluster 4 with an empty dir cluster.
	binary.LittleEndian.PutUint32(image[fatBase+4*4:], 0xFFFFFFFF)
	root := image[int(info.RootDirOffset(0)):]
	writeExFATEntrySetWithAttr(root[96:], "subdir", 4, exfatAttrDir, 0)
	root[192] = 0x00
	// Write empty dir cluster for subdir.
	dirOff := dataBase + (4-2)*cs
	image[dirOff] = 0x00

	subDirOff := int64(info.ClusterHeapOffsetBytes(0)) + int64(4-2)*int64(info.ClusterSize())
	fs := newMockFSWithErrors(image, info, 0, func(off int64) error {
		if off == subDirOff {
			return errors.New("injected subdir read error")
		}
		return nil
	}, nil)
	if err := fs.Rename("/file.txt", "/subdir/new.txt"); err == nil {
		t.Fatal("Rename cross-dir read error = nil, want error")
	}
}

func TestRenameCrossDirWriteError(t *testing.T) {
	image, info := buildExFATImageWithEntry(t)
	fatBase := int(info.FATOffsetBytes(0))
	cs := int(info.ClusterSize())
	dataBase := int(info.ClusterHeapOffsetBytes(0))
	// Add a subdirectory at cluster 4.
	binary.LittleEndian.PutUint32(image[fatBase+4*4:], 0xFFFFFFFF)
	root := image[int(info.RootDirOffset(0)):]
	writeExFATEntrySetWithAttr(root[96:], "subdir", 4, exfatAttrDir, 0)
	root[192] = 0x00
	dirOff := dataBase + (4-2)*cs
	image[dirOff] = 0x00

	// Inject a write error when writing old (root dir) cluster after removing the entry.
	rootClusterOff := info.RootDirOffset(0)
	writeCount := 0
	fs := newMockFSWithErrors(image, info, 0, nil, func(off int64) error {
		if off == rootClusterOff {
			writeCount++
			// Fail on the first write (after deleting old entry from root dir).
			if writeCount == 1 {
				return errors.New("injected old-dir write error")
			}
		}
		return nil
	})
	if err := fs.Rename("/file.txt", "/subdir/new.txt"); err == nil {
		t.Fatal("Rename cross-dir write error = nil, want error")
	}
}

func TestRenameCrossDirDestFull(t *testing.T) {
	image, info := buildExFATImageWithEntry(t)
	fatBase := int(info.FATOffsetBytes(0))
	cs := int(info.ClusterSize())
	dataBase := int(info.ClusterHeapOffsetBytes(0))
	// Add a subdirectory at cluster 4. Fill its directory cluster completely.
	binary.LittleEndian.PutUint32(image[fatBase+4*4:], 0xFFFFFFFF)
	root := image[int(info.RootDirOffset(0)):]
	writeExFATEntrySetWithAttr(root[96:], "subdir", 4, exfatAttrDir, 0)
	root[192] = 0x00
	// Fill the subdirectory cluster with in-use entries (no free slots, no end marker).
	dirBuf := image[dataBase+(4-2)*cs : dataBase+(4-2)*cs+cs]
	for off := 0; off+96 <= cs; off += 96 {
		writeExFATEntrySetWithAttr(dirBuf[off:], string([]byte{'A' + byte(off/96)}), uint32(10+off/96), 0x20, 1)
	}
	for off := (cs / 96) * 96; off < cs; off++ {
		dirBuf[off] = 0x81
	}

	fs := newMockFSWithErrors(image, info, 0, nil, nil)
	if err := fs.Rename("/file.txt", "/subdir/new.txt"); err == nil {
		t.Fatal("Rename cross-dir dest full = nil, want error")
	}
}

func TestWriteFileEmptyName(t *testing.T) {
	// A double-slash path resolves to an empty entry name via getParentDir.
	// WriteFile must reject it rather than creating a malformed directory entry.
	path := exfatTestImage(t, func(root []byte) { root[0] = 0x00 }, nil, nil)
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()
	if err := fs.WriteFile("//", nil, 0o644); err == nil {
		t.Fatal("WriteFile(//) error = nil, want error")
	}
}

func TestWriteDirBufNonAligned(t *testing.T) {
	// Write a buffer whose length is between one and two cluster sizes.
	// This triggers the "truncate to buf length" path inside writeDirBuf.
	image := make([]byte, 1024*1024)
	boot := defaultExFATBootSector()
	copy(image, boot)
	info, _ := readInfo(bytes.NewReader(boot), 0)
	fatBase := int(info.FATOffsetBytes(0))
	cs := int(info.ClusterSize())
	rootCluster := info.RootDirectoryCluster
	nextCluster := rootCluster + 1
	// Build FAT chain: rootCluster → nextCluster → EOC.
	binary.LittleEndian.PutUint32(image[fatBase+int(rootCluster)*4:], nextCluster)
	binary.LittleEndian.PutUint32(image[fatBase+int(nextCluster)*4:], 0xFFFFFFFF)

	fs := newMockFSWithErrors(image, info, 0, nil, nil)
	// A buf slightly larger than one cluster triggers the end-of-buf truncation.
	buf := make([]byte, cs+1)
	if err := fs.writeDirBuf(rootCluster, buf); err != nil {
		t.Fatalf("writeDirBuf non-aligned: %v", err)
	}
}

func TestDeleteDirRecursiveFreeChainError(t *testing.T) {
	// parent (cluster 4) contains file.txt at cluster 6.
	// Inject a write error when freeing cluster 6 inside deleteAllContents.
	image := make([]byte, 1024*1024)
	boot := defaultExFATBootSector()
	copy(image, boot)
	info, _ := readInfo(bytes.NewReader(boot), 0)
	fatBase := int(info.FATOffsetBytes(0))
	binary.LittleEndian.PutUint32(image[fatBase+int(info.RootDirectoryCluster)*4:], 0xFFFFFFFF)
	binary.LittleEndian.PutUint32(image[fatBase+4*4:], 0xFFFFFFFF)
	binary.LittleEndian.PutUint32(image[fatBase+6*4:], 0xFFFFFFFF)
	root := image[int(info.RootDirOffset(0)):]
	writeExFATEntrySetWithAttr(root[0:], "parent", 4, exfatAttrDir, 0)
	root[96] = 0x00
	dataBase := int(info.ClusterHeapOffsetBytes(0))
	cs := int(info.ClusterSize())
	parentData := image[dataBase+(4-2)*cs:]
	writeExFATEntrySetWithAttr(parentData[0:], "file.txt", 6, 0x20, 5)
	parentData[96] = 0x00

	fs := newMockFSWithErrors(image, info, 0, nil, func(off int64) error {
		if off == int64(fatBase)+6*4 {
			return errors.New("freeChain error for child file")
		}
		return nil
	})
	if err := fs.DeleteDir("/parent"); err == nil {
		t.Fatal("DeleteDir recursive freeChain error = nil, want error")
	}
}

func TestDeleteDirRecursiveSubdirError(t *testing.T) {
	// parent (cluster 4) contains subdir (cluster 7).
	// subdir (cluster 7) contains file.txt at cluster 9.
	// Injecting a write error for cluster 9 causes deleteAllContents(7) to fail,
	// which should propagate back through deleteAllContents(4).
	image := make([]byte, 1024*1024)
	boot := defaultExFATBootSector()
	copy(image, boot)
	info, _ := readInfo(bytes.NewReader(boot), 0)
	fatBase := int(info.FATOffsetBytes(0))
	binary.LittleEndian.PutUint32(image[fatBase+int(info.RootDirectoryCluster)*4:], 0xFFFFFFFF)
	binary.LittleEndian.PutUint32(image[fatBase+4*4:], 0xFFFFFFFF)
	binary.LittleEndian.PutUint32(image[fatBase+7*4:], 0xFFFFFFFF)
	binary.LittleEndian.PutUint32(image[fatBase+9*4:], 0xFFFFFFFF)
	root := image[int(info.RootDirOffset(0)):]
	writeExFATEntrySetWithAttr(root[0:], "parent", 4, exfatAttrDir, 0)
	root[96] = 0x00
	dataBase := int(info.ClusterHeapOffsetBytes(0))
	cs := int(info.ClusterSize())
	parentData := image[dataBase+(4-2)*cs:]
	writeExFATEntrySetWithAttr(parentData[0:], "subdir", 7, exfatAttrDir, 0)
	parentData[96] = 0x00
	subdirData := image[dataBase+(7-2)*cs:]
	writeExFATEntrySetWithAttr(subdirData[0:], "file.txt", 9, 0x20, 3)
	subdirData[96] = 0x00

	fs := newMockFSWithErrors(image, info, 0, nil, func(off int64) error {
		if off == int64(fatBase)+9*4 {
			return errors.New("freeChain error for nested file")
		}
		return nil
	})
	if err := fs.DeleteDir("/parent"); err == nil {
		t.Fatal("DeleteDir recursive subdir error = nil, want error")
	}
}
