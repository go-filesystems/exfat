package filesystem_exfat

import (
	"encoding/binary"
	"path/filepath"
	"testing"
)

// Pins for the defects go-filesystems/kerneljudge found against the Linux
// kernel and fsck.exfat (#34, #35, #36).

func TestFormatLargeVolumeScalesTheCluster(t *testing.T) {
	for _, mib := range []int64{64, 128, 256, 512} {
		img := filepath.Join(t.TempDir(), "a.img")
		f, err := Format(img, mib<<20, FormatConfig{})
		if err != nil {
			t.Fatalf("%d MiB: %v", mib, err)
		}
		info := f.(*exfatFS).info
		if bitmap := (uint64(info.ClusterCount) + 7) / 8; bitmap > info.ClusterSize() {
			t.Errorf("%d MiB: bitmap %d bytes does not fit one %d-byte cluster", mib, bitmap, info.ClusterSize())
		}
		if mib <= 128 && info.ClusterSize() != 4096 {
			t.Errorf("%d MiB: cluster %d, the 4 KiB default is expected up to 128 MiB", mib, info.ClusterSize())
		}
		if err := f.WriteFile("/x", []byte("hello"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got, err := f.ReadFile("/x"); err != nil || string(got) != "hello" {
			t.Fatalf("%d MiB: read back %q, %v", mib, got, err)
		}
		f.Close()
	}
}

func TestMkDirRecordsTheDirectoryAllocation(t *testing.T) {
	f, err := Format(filepath.Join(t.TempDir(), "a.img"), 64<<20, FormatConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fs := f.(*exfatFS)
	if err := f.MkDir("/d", 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := fs.readDirBuf(fs.info.RootDirectoryCluster)
	if err != nil {
		t.Fatal(err)
	}
	off, _ := exfatFindEntry(root, "d")
	if off < 0 {
		t.Fatal("entry not found")
	}
	stream := root[off+dirEntrySize:]
	for _, field := range []int{8, 24} { // ValidDataLength, DataLength
		if got := binary.LittleEndian.Uint64(stream[field:]); got != fs.info.ClusterSize() {
			t.Errorf("stream field at %d = %d, want one cluster (%d): a size-0 directory reads as empty in the kernel", field, got, fs.info.ClusterSize())
		}
	}
}

func TestNameHashFollowsTheVolumeTable(t *testing.T) {
	f, err := Format(filepath.Join(t.TempDir(), "a.img"), 64<<20, FormatConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fs := f.(*exfatFS)
	// Our table folds ASCII only: "é" must hash as "é", not as "É".
	if got, want := fs.nameHash("données"), hashWithTable("DONNéES", identityUpcase()); got != want {
		t.Errorf("hash of données = %#x, want %#x (ASCII folded, é untouched)", got, want)
	}
	if fs.nameHash("données") == hashWithTable("DONNÉES", identityUpcase()) {
		t.Error("the hash was made with Unicode folding of é")
	}
	// A volume with a full table (mkfs.exfat's) folds é to É: decode it.
	raw := make([]byte, 0, 8)
	raw = binary.LittleEndian.AppendUint16(raw, 0xFFFF)
	raw = binary.LittleEndian.AppendUint16(raw, 0xE9) // 0xE9 identity entries
	raw = binary.LittleEndian.AppendUint16(raw, 0xC9) // U+00E9 -> U+00C9
	tbl := decodeUpcase(raw)
	if tbl[0xE9] != 0xC9 || tbl[0xE8] != 0xE8 || tbl[0xEA] != 0xEA {
		t.Fatalf("decoded table: E8 %#x E9 %#x EA %#x", tbl[0xE8], tbl[0xE9], tbl[0xEA])
	}
	if hashWithTable("é", tbl) != hashWithTable("É", identityUpcase()) {
		t.Error("a table that maps é to É did not change the hash")
	}
}
