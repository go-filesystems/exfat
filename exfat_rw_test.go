package filesystem_exfat

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestReadFile(t *testing.T) {
	path := exfatTestImage(t, func(root []byte) {
		writeExFATEntrySetWithAttr(root[0:], "README.TXT", 3, 0x20, 5)
		writeExFATEntrySetWithAttr(root[96:], "SUBDIR", 4, exfatAttrDir, 0)
		writeExFATEntrySetWithAttr(root[192:], "EMPTY.TXT", 0, 0x20, 0)
		root[288] = 0x00
	}, map[uint32]uint32{3: 0xFFFFFFFF, 4: 0xFFFFFFFF}, map[uint32][]byte{3: []byte("hello")})

	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	if _, err := fs.ReadFile("/"); err == nil {
		t.Fatal("ReadFile(/) error = nil, want error")
	}
	if _, err := fs.ReadFile("/nested/f"); err == nil {
		t.Fatal("ReadFile(nested) error = nil, want error")
	}
	if _, err := fs.ReadFile("/SUBDIR"); err == nil {
		t.Fatal("ReadFile(dir) error = nil, want error")
	}
	if _, err := fs.ReadFile("/missing.txt"); err == nil {
		t.Fatal("ReadFile(missing) error = nil, want error")
	}
	// empty file (cluster=0)
	data, err := fs.ReadFile("/empty.txt")
	if err != nil {
		t.Fatalf("ReadFile(empty): %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("ReadFile(empty) = %v, want empty", data)
	}
	// success
	data, err = fs.ReadFile("/readme.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("ReadFile = %q, want %q", data, "hello")
	}
}

func TestReadFileIOError(t *testing.T) {
	path := exfatTestImage(t, func(root []byte) {
		writeExFATEntrySetWithAttr(root[0:], "DATA.TXT", 3, 0x20, 5)
		root[96] = 0x00
	}, map[uint32]uint32{3: 0xFFFFFFFF}, nil)
	fs := openTestFS(t, path)
	fs.f.Close()
	if _, err := fs.ReadFile("/data.txt"); err == nil {
		t.Fatal("ReadFile with closed file error = nil, want error")
	}
}

func TestWriteFile(t *testing.T) {
	path := exfatTestImage(t, func(root []byte) { root[0] = 0x00 }, nil, nil)

	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	if err := fs.WriteFile("/", nil, 0o644); err == nil {
		t.Fatal("WriteFile(/) error = nil, want error")
	}
	if err := fs.WriteFile("noabs.txt", nil, 0o644); err == nil {
		t.Fatal("WriteFile(no leading slash) error = nil, want error")
	}
	if err := fs.WriteFile("/nested/f", nil, 0o644); err == nil {
		t.Fatal("WriteFile(nested) error = nil, want error")
	}

	if err := fs.WriteFile("/hello.txt", []byte("world"), 0o644); err != nil {
		t.Fatalf("WriteFile(create): %v", err)
	}
	data, err := fs.ReadFile("/hello.txt")
	if err != nil {
		t.Fatalf("ReadFile after WriteFile: %v", err)
	}
	if string(data) != "world" {
		t.Fatalf("data = %q, want %q", data, "world")
	}

	if err := fs.WriteFile("/hello.txt", []byte("updated"), 0o444); err != nil {
		t.Fatalf("WriteFile(overwrite): %v", err)
	}
	data, err = fs.ReadFile("/hello.txt")
	if err != nil {
		t.Fatalf("ReadFile after overwrite: %v", err)
	}
	if string(data) != "updated" {
		t.Fatalf("overwritten data = %q, want %q", data, "updated")
	}

	if err := fs.WriteFile("/empty.txt", nil, 0o644); err != nil {
		t.Fatalf("WriteFile(zero-length): %v", err)
	}
	data, err = fs.ReadFile("/empty.txt")
	if err != nil {
		t.Fatalf("ReadFile(zero-length): %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("ReadFile(zero-length) = %v, want empty", data)
	}
}

func TestWriteFileMultiCluster(t *testing.T) {
	path := exfatTestImage(t, func(root []byte) { root[0] = 0x00 }, nil, nil)
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	bigData := make([]byte, 5000)
	for i := range bigData {
		bigData[i] = byte(i % 251)
	}
	if err := fs.WriteFile("/big.txt", bigData, 0o644); err != nil {
		t.Fatalf("WriteFile(multi-cluster): %v", err)
	}
	got, err := fs.ReadFile("/big.txt")
	if err != nil {
		t.Fatalf("ReadFile(multi-cluster): %v", err)
	}
	if len(got) != len(bigData) || got[4999] != bigData[4999] {
		t.Fatal("multi-cluster data mismatch")
	}
	if err := fs.WriteFile("/big.txt", []byte("small"), 0o644); err != nil {
		t.Fatalf("WriteFile(overwrite big): %v", err)
	}
	got2, err := fs.ReadFile("/big.txt")
	if err != nil {
		t.Fatalf("ReadFile after overwrite: %v", err)
	}
	if string(got2) != "small" {
		t.Fatalf("overwrite data = %q, want small", got2)
	}
}

func TestWriteFileFullFAT(t *testing.T) {
	image := make([]byte, 1024*1024)
	boot := defaultExFATBootSector()
	copy(image, boot)
	info, _ := readInfo(bytes.NewReader(boot), 0)
	fatBase := int(info.FATOffsetBytes(0))
	fatBytes := int(info.FATLength) * int(info.BytesPerSector())
	for i := 0; i < fatBytes; i++ {
		image[fatBase+i] = 0xFF
	}
	root := image[int(info.RootDirOffset(0)):]
	root[0] = 0x00

	path := filepath.Join(t.TempDir(), "exfat-full.img")
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()
	if err := fs.WriteFile("/file.txt", []byte("data"), 0o644); err == nil {
		t.Fatal("WriteFile on full FAT error = nil, want error")
	}
}

func TestWriteFileIOError(t *testing.T) {
	path := exfatTestImage(t, func(root []byte) { root[0] = 0x00 }, nil, nil)
	fs := openTestFS(t, path)
	fs.f.Close()
	if err := fs.WriteFile("/test.txt", []byte("x"), 0o644); err == nil {
		t.Fatal("WriteFile with closed file error = nil, want error")
	}
}

func TestReadLink(t *testing.T) {
	path := exfatTestImage(t, func(root []byte) { root[0] = 0x00 }, nil, nil)
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()
	if _, err := fs.ReadLink("/anything"); err == nil {
		t.Fatal("ReadLink error = nil, want error")
	}
	if _, err := fs.ReadLink("/"); err == nil {
		t.Fatal("ReadLink(/) error = nil, want error")
	}
}

func TestMkDir(t *testing.T) {
	path := exfatTestImage(t, func(root []byte) { root[0] = 0x00 }, nil, nil)
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	if err := fs.MkDir("/", 0o755); err == nil {
		t.Fatal("MkDir(/) error = nil, want error")
	}
	if err := fs.MkDir("/nested/dir", 0o755); err == nil {
		t.Fatal("MkDir(nested) error = nil, want error")
	}
	if err := fs.MkDir("/mydir", 0o755); err != nil {
		t.Fatalf("MkDir: %v", err)
	}
	if err := fs.MkDir("/mydir", 0o755); err == nil {
		t.Fatal("MkDir duplicate error = nil, want error")
	}
	if err := fs.MkDir("/rodir", 0o555); err != nil {
		t.Fatalf("MkDir(ro): %v", err)
	}
	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir after MkDir: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Name() == "mydir" {
			found = true
		}
	}
	if !found {
		t.Fatal("MkDir: mydir not found in root dir")
	}
}

func TestMkDirIOError(t *testing.T) {
	path := exfatTestImage(t, func(root []byte) { root[0] = 0x00 }, nil, nil)
	fs := openTestFS(t, path)
	fs.f.Close()
	if err := fs.MkDir("/mydir", 0o755); err == nil {
		t.Fatal("MkDir with closed file error = nil, want error")
	}
}

func TestMkDirFullFAT(t *testing.T) {
	image := make([]byte, 1024*1024)
	boot := defaultExFATBootSector()
	copy(image, boot)
	info, _ := readInfo(bytes.NewReader(boot), 0)
	fatBase := int(info.FATOffsetBytes(0))
	fatBytes := int(info.FATLength) * int(info.BytesPerSector())
	for i := 0; i < fatBytes; i++ {
		image[fatBase+i] = 0xFF
	}
	root := image[int(info.RootDirOffset(0)):]
	root[0] = 0x00
	path := filepath.Join(t.TempDir(), "exfat-full2.img")
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()
	if err := fs.MkDir("/newdir", 0o755); err == nil {
		t.Fatal("MkDir on full FAT error = nil, want error")
	}
}

func TestDeleteFile(t *testing.T) {
	path := exfatTestImage(t, func(root []byte) {
		writeExFATEntrySetWithAttr(root[0:], "file.txt", 3, 0x20, 10)
		writeExFATEntrySetWithAttr(root[96:], "subdir", 4, exfatAttrDir, 0)
		root[192] = 0x00
	}, map[uint32]uint32{3: 0xFFFFFFFF, 4: 0xFFFFFFFF}, nil)

	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	if err := fs.DeleteFile("/"); err == nil {
		t.Fatal("DeleteFile(/) error = nil, want error")
	}
	if err := fs.DeleteFile("/n/f"); err == nil {
		t.Fatal("DeleteFile(nested) error = nil, want error")
	}
	if err := fs.DeleteFile("/missing.txt"); err == nil {
		t.Fatal("DeleteFile(missing) error = nil, want error")
	}
	if err := fs.DeleteFile("/subdir"); err == nil {
		t.Fatal("DeleteFile(dir) error = nil, want error")
	}
	if err := fs.DeleteFile("/file.txt"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir after delete: %v", err)
	}
	for _, e := range entries {
		if e.Name() == "file.txt" {
			t.Fatal("file still present after DeleteFile")
		}
	}
}

func TestDeleteFileIOError(t *testing.T) {
	path := exfatTestImage(t, func(root []byte) {
		writeExFATEntrySetWithAttr(root[0:], "file.txt", 3, 0x20, 5)
		root[96] = 0x00
	}, map[uint32]uint32{3: 0xFFFFFFFF}, nil)
	fs := openTestFS(t, path)
	fs.f.Close()
	if err := fs.DeleteFile("/file.txt"); err == nil {
		t.Fatal("DeleteFile with closed file error = nil, want error")
	}
}

func TestDeleteDir(t *testing.T) {
	// cluster 3: empty dir, cluster 4: non-empty dir
	emptyCluster := make([]byte, 4096)
	emptyCluster[0] = 0x00
	fullCluster := make([]byte, 4096)
	fullCluster[0] = exfatEntryFile
	fullCluster[1] = 2
	fullCluster[0] |= 0x80 // in-use

	path := exfatTestImage(t, func(root []byte) {
		writeExFATEntrySetWithAttr(root[0:], "emptydir", 3, exfatAttrDir, 0)
		writeExFATEntrySetWithAttr(root[96:], "fulldir", 4, exfatAttrDir, 0)
		writeExFATEntrySetWithAttr(root[192:], "file.txt", 5, 0x20, 5)
		root[288] = 0x00
	}, map[uint32]uint32{3: 0xFFFFFFFF, 4: 0xFFFFFFFF, 5: 0xFFFFFFFF},
		map[uint32][]byte{3: emptyCluster, 4: fullCluster})

	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	if err := fs.DeleteDir("/"); err == nil {
		t.Fatal("DeleteDir(/) error = nil, want error")
	}
	if err := fs.DeleteDir("/n/d"); err == nil {
		t.Fatal("DeleteDir(nested) error = nil, want error")
	}
	if err := fs.DeleteDir("/missing"); err == nil {
		t.Fatal("DeleteDir(missing) error = nil, want error")
	}
	if err := fs.DeleteDir("/file.txt"); err == nil {
		t.Fatal("DeleteDir(file) error = nil, want error")
	}
	if err := fs.DeleteDir("/fulldir"); err != nil {
		t.Fatalf("DeleteDir(recursive): %v", err)
	}
	if err := fs.DeleteDir("/emptydir"); err != nil {
		t.Fatalf("DeleteDir(empty): %v", err)
	}
	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir after DeleteDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() == "emptydir" {
			t.Fatal("dir still present after DeleteDir")
		}
	}
}

func TestDeleteDirWithNoCluster(t *testing.T) {
	path := exfatTestImage(t, func(root []byte) {
		writeExFATEntrySetWithAttr(root[0:], "zerodir", 0, exfatAttrDir, 0)
		root[96] = 0x00
	}, nil, nil)
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()
	if err := fs.DeleteDir("/zerodir"); err != nil {
		t.Fatalf("DeleteDir(cluster=0): %v", err)
	}
}

func TestDeleteDirIOError(t *testing.T) {
	path := exfatTestImage(t, func(root []byte) {
		writeExFATEntrySetWithAttr(root[0:], "mydir", 3, exfatAttrDir, 0)
		root[96] = 0x00
	}, map[uint32]uint32{3: 0xFFFFFFFF}, nil)
	fs := openTestFS(t, path)
	fs.f.Close()
	if err := fs.DeleteDir("/mydir"); err == nil {
		t.Fatal("DeleteDir with closed file error = nil, want error")
	}
}

func TestDeleteDirClusterReadError(t *testing.T) {
	boot := defaultExFATBootSector()
	info, _ := readInfo(bytes.NewReader(boot), 0)
	imageSize := int(info.RootDirOffset(0)) + int(info.ClusterSize())
	image := make([]byte, imageSize)
	copy(image, boot)
	fatBase := int(info.FATOffsetBytes(0))
	binary.LittleEndian.PutUint32(image[fatBase+int(info.RootDirectoryCluster)*4:], 0xFFFFFFFF)
	binary.LittleEndian.PutUint32(image[fatBase+3*4:], 0xFFFFFFFF)
	root := image[int(info.RootDirOffset(0)):]
	writeExFATEntrySetWithAttr(root[0:], "mydir", 3, exfatAttrDir, 0)
	root[96] = 0x00

	path := filepath.Join(t.TempDir(), "exfat-trunc.img")
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()
	if err := fs.DeleteDir("/mydir"); err == nil {
		t.Fatal("DeleteDir on truncated image error = nil, want cluster read error")
	}
}

func TestDeleteDirRecursive(t *testing.T) {
	// parent (cluster 4) contains:
	//   [0]   0x81  — non-file in-use entry  → exercises typ != exfatEntryFile
	//   [32]  0x85 with secondaryCount=0     → exercises secondaryCount < 2
	//   [64]  valid file.txt  at cluster 6   → exercises freeChain inside deleteAllContents
	//   [160] valid subdir   at cluster 7    → exercises recursive deleteAllContents
	//   [256] 0x85 with secondaryCount=2, stream[0]=0 → exercises stream check failure
	//   [352] 0x00                           → exfatEntryEnd break (implicit from make)
	parentCluster := make([]byte, 4096)
	parentCluster[0] = 0x81 // in-use non-file
	parentCluster[32] = exfatEntryFile
	parentCluster[33] = 0 // secondaryCount=0 < 2
	writeExFATEntrySetWithAttr(parentCluster[64:], "file.txt", 6, 0x20, 5)
	writeExFATEntrySetWithAttr(parentCluster[160:], "subdir", 7, exfatAttrDir, 0)
	parentCluster[256] = exfatEntryFile
	parentCluster[257] = 2 // secondaryCount=2, stream at [288]=0 ≠ exfatEntryStream

	subdirCluster := make([]byte, 4096)
	subdirCluster[0] = exfatEntryEnd

	path := exfatTestImage(t, func(root []byte) {
		writeExFATEntrySetWithAttr(root[0:], "parent", 4, exfatAttrDir, 0)
		root[96] = exfatEntryEnd
	}, map[uint32]uint32{4: 0xFFFFFFFF, 6: 0xFFFFFFFF, 7: 0xFFFFFFFF},
		map[uint32][]byte{4: parentCluster, 6: []byte("hello"), 7: subdirCluster})

	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	if err := fs.DeleteDir("/parent"); err != nil {
		t.Fatalf("DeleteDir(recursive): %v", err)
	}
	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir after recursive delete: %v", err)
	}
	for _, e := range entries {
		if e.Name() == "parent" {
			t.Fatal("parent still present after recursive DeleteDir")
		}
	}
}

func TestRename(t *testing.T) {
	path := exfatTestImage(t, func(root []byte) {
		writeExFATEntrySetWithAttr(root[0:], "old.txt", 3, 0x20, 5)
		writeExFATEntrySetWithAttr(root[96:], "other.txt", 4, 0x20, 3)
		root[192] = 0x00
	}, map[uint32]uint32{3: 0xFFFFFFFF, 4: 0xFFFFFFFF},
		map[uint32][]byte{3: []byte("hello"), 4: []byte("bye")})

	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	if err := fs.Rename("/", "/new"); err == nil {
		t.Fatal("Rename(root old) error = nil, want error")
	}
	if err := fs.Rename("/old.txt", "/"); err == nil {
		t.Fatal("Rename(root new) error = nil, want error")
	}
	if err := fs.Rename("/n/a", "/b"); err == nil {
		t.Fatal("Rename(nested old) error = nil, want error")
	}
	if err := fs.Rename("/old.txt", "/n/b"); err == nil {
		t.Fatal("Rename(nested new) error = nil, want error")
	}
	if err := fs.Rename("/missing.txt", "/new.txt"); err == nil {
		t.Fatal("Rename(missing) error = nil, want error")
	}
	// same name (case-insensitive no-op)
	if err := fs.Rename("/old.txt", "/OLD.TXT"); err != nil {
		t.Fatalf("Rename(same name): %v", err)
	}
	// success
	if err := fs.Rename("/old.txt", "/renamed.txt"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	data, err := fs.ReadFile("/renamed.txt")
	if err != nil {
		t.Fatalf("ReadFile after Rename: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("renamed data = %q, want %q", data, "hello")
	}
	// rename replacing existing
	if err := fs.Rename("/other.txt", "/renamed.txt"); err != nil {
		t.Fatalf("Rename(replace): %v", err)
	}
	data, err = fs.ReadFile("/renamed.txt")
	if err != nil {
		t.Fatalf("ReadFile after second rename: %v", err)
	}
	if string(data) != "bye" {
		t.Fatalf("replaced data = %q, want %q", data, "bye")
	}
}

func TestRenameIOError(t *testing.T) {
	path := exfatTestImage(t, func(root []byte) {
		writeExFATEntrySetWithAttr(root[0:], "old.txt", 3, 0x20, 5)
		root[96] = 0x00
	}, map[uint32]uint32{3: 0xFFFFFFFF}, nil)
	fs := openTestFS(t, path)
	fs.f.Close()
	if err := fs.Rename("/old.txt", "/new.txt"); err == nil {
		t.Fatal("Rename with closed file error = nil, want error")
	}
}

func TestListDirOnFile(t *testing.T) {
	path := exfatTestImage(t, func(root []byte) {
		writeExFATEntrySetWithAttr(root[0:], "file.txt", 3, 0x20, 5)
		root[96] = 0x00
	}, map[uint32]uint32{3: 0xFFFFFFFF}, map[uint32][]byte{3: []byte("hello")})

	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	// ListDir on a file must return an error.
	if _, err := fs.ListDir("/file.txt"); err == nil {
		t.Fatal("ListDir on file error = nil, want error")
	}
	// ReadFile through a non-directory intermediate must return an error (resolvePath case 3c).
	if _, err := fs.ReadFile("/file.txt/nested"); err == nil {
		t.Fatal("ReadFile via non-dir path error = nil, want error")
	}
}

func TestRenameCrossDirectory(t *testing.T) {
	emptyDirCluster := make([]byte, 4096)
	emptyDirCluster[0] = 0x00
	path := exfatTestImage(t, func(root []byte) {
		writeExFATEntrySetWithAttr(root[0:], "subdir", 4, exfatAttrDir, 0)
		writeExFATEntrySetWithAttr(root[96:], "old.txt", 3, 0x20, 5)
		root[192] = 0x00
	}, map[uint32]uint32{3: 0xFFFFFFFF, 4: 0xFFFFFFFF},
		map[uint32][]byte{3: []byte("hello"), 4: emptyDirCluster})

	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	// Cross-directory rename: root → subdir.
	if err := fs.Rename("/old.txt", "/subdir/new.txt"); err != nil {
		t.Fatalf("Rename cross-dir: %v", err)
	}
	data, err := fs.ReadFile("/subdir/new.txt")
	if err != nil {
		t.Fatalf("ReadFile after cross-dir Rename: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("cross-dir renamed data = %q, want hello", data)
	}
}

func TestParentNotDirectory(t *testing.T) {
	path := exfatTestImage(t, func(root []byte) {
		writeExFATEntrySetWithAttr(root[0:], "file.txt", 3, 0x20, 5)
		root[96] = 0x00
	}, map[uint32]uint32{3: 0xFFFFFFFF}, nil)

	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	// Writing into a path whose parent is a file must fail (getParentDir line F).
	if err := fs.WriteFile("/file.txt/child", nil, 0o644); err == nil {
		t.Fatal("WriteFile into non-dir parent error = nil, want error")
	}
}
