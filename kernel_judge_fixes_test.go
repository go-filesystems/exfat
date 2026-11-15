package filesystem_exfat

import (
	"encoding/binary"
	"path/filepath"
	"runtime"
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

func TestWrittenHashUsesTheVolumeTableOnEveryPath(t *testing.T) {
	f, err := Format(filepath.Join(t.TempDir(), "a.img"), 64<<20, FormatConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fs := f.(*exfatFS)
	tbl := fallbackUpcase()
	tbl[0xE9] = 0xC9 // pretend the volume folds é to É
	fs.upcase = tbl
	hashOnDisk := func(name string) uint16 {
		root, err := fs.readDirBuf(fs.info.RootDirectoryCluster)
		if err != nil {
			t.Fatal(err)
		}
		off, _ := exfatFindEntry(root, name)
		if off < 0 {
			t.Fatalf("%s not found", name)
		}
		return binary.LittleEndian.Uint16(root[off+dirEntrySize+4:])
	}
	if err := f.WriteFile("/é.txt", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.MkDir("/éd", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.Rename("/é.txt", "/é2.txt"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"é2.txt", "éd"} {
		if got, want := hashOnDisk(name), hashWithTable(name, tbl); got != want {
			t.Errorf("%s: on-disk hash %#x, want %#x under the volume table", name, got, want)
		}
	}
}

func TestMissingTableFallsBackToTheKernelsFolding(t *testing.T) {
	tbl := fallbackUpcase()
	if tbl['a'] != 'A' || tbl[0xE9] != 0xC9 || tbl[0xF7] != 0xF7 || tbl['1'] != '1' {
		t.Fatal("the fallback table does not fold ASCII and Latin-1")
	}
	if got := decodeUpcase([]byte{0xFF, 0xFF}); got[0xFFFF] != 0xFFFF || got[5] != 5 {
		t.Error("a trailing 0xFFFF changed the table")
	}
}

// A Stat or ReadFile resolves its path by reading the directories on it; the
// bound on the chain length used to be tested with safeio.MakeBytes, which
// allocates and zeroes that many bytes: a metadata walk paid for a buffer of the
// directory ceiling per path component (#39, 25x slower than the kernel).
func TestStatDoesNotAllocateTheDirectoryCeiling(t *testing.T) {
	f, err := Format(filepath.Join(t.TempDir(), "a.img"), 256<<20, FormatConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, p := range []string{"/d", "/d/e"} {
		if err := f.MkDir(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.WriteFile("/d/e/f", []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	const n = 200
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < n; i++ {
		if _, err := f.Stat("/d/e/f"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.ReadFile("/d/e/f"); err != nil {
			t.Fatal(err)
		}
	}
	runtime.ReadMemStats(&after)
	if per := (after.TotalAlloc - before.TotalAlloc) / (2 * n); per > 256<<10 {
		t.Errorf("a Stat/ReadFile of a 3-component path allocates %d bytes on average; it should be a few directory clusters", per)
	}
}
