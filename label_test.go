package filesystem_exfat

import (
	"path/filepath"
	"strings"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

func openFreshExfat(t *testing.T) (*exfatFS, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(p, exfatTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	return fs.(*exfatFS), p
}

func TestExfatSetLabel_Roundtrip(t *testing.T) {
	fs, _ := openFreshExfat(t)
	defer fs.Close()
	if got := fs.Label(); got != "" {
		t.Errorf("default Label() = %q, want empty", got)
	}
	if err := fs.SetLabel("USBSTICK"); err != nil {
		t.Fatalf("SetLabel: %v", err)
	}
	if got := fs.Label(); got != "USBSTICK" {
		t.Errorf("Label() = %q, want %q", got, "USBSTICK")
	}
}

func TestExfatSetLabel_PersistsAcrossReopen(t *testing.T) {
	fs, img := openFreshExfat(t)
	if err := fs.SetLabel("PERSIST"); err != nil {
		t.Fatalf("SetLabel: %v", err)
	}
	fs.Close()

	fs2, err := Open(img, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs2.Close()
	l, ok := fs2.(filesystem.Labeller)
	if !ok {
		t.Fatal("reopened exfat does not implement Labeller")
	}
	if got := l.Label(); got != "PERSIST" {
		t.Errorf("after reopen Label() = %q, want %q", got, "PERSIST")
	}
}

func TestExfatSetLabel_FormatConfigSeedsLabel(t *testing.T) {
	p := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(p, exfatTestSize, FormatConfig{Label: "SEEDED"})
	if err != nil {
		t.Fatalf("Format with Label: %v", err)
	}
	if got := fs.(*exfatFS).Label(); got != "SEEDED" {
		t.Errorf("seeded Label() = %q, want %q", got, "SEEDED")
	}
	fs.Close()
	// And confirm the label survives a reopen — proves the entry was
	// actually written to root[0] and is not just in-memory.
	fs2, err := Open(p, -1)
	if err != nil {
		t.Fatalf("Open after seeded Format: %v", err)
	}
	defer fs2.Close()
	if got := fs2.(*exfatFS).Label(); got != "SEEDED" {
		t.Errorf("seeded label not persisted: %q after reopen", got)
	}
}

func TestExfatSetLabel_RejectsTooLong(t *testing.T) {
	fs, _ := openFreshExfat(t)
	defer fs.Close()
	before := fs.Label()
	// MaxLabelLen is 11 UTF-16 code units — anything past that errors.
	if err := fs.SetLabel(strings.Repeat("X", MaxLabelLen+1)); err == nil {
		t.Error("SetLabel with oversize input unexpectedly succeeded")
	}
	if after := fs.Label(); after != before {
		t.Errorf("Label() changed after rejected SetLabel: %q -> %q", before, after)
	}
}

func TestExfatSetLabel_EmptyMarksUnusedEntry(t *testing.T) {
	fs, img := openFreshExfat(t)
	if err := fs.SetLabel("temporary"); err != nil {
		t.Fatalf("SetLabel: %v", err)
	}
	if err := fs.SetLabel(""); err != nil {
		t.Fatalf("SetLabel empty: %v", err)
	}
	fs.Close()

	// On-disk entry 0 must now be 0x03 (volume label, not-in-use).
	fs2, err := Open(img, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs2.Close()
	if got := fs2.(*exfatFS).Label(); got != "" {
		t.Errorf("after clearing, Label() = %q, want empty", got)
	}

	// Also verify the Volume Label slot in the root directory is now
	// stored as 0x03 (not-in-use) — sanity-check that the on-disk format
	// is the kernel-canonical one. The label entry isn't necessarily at
	// offset 0 anymore (the Bitmap and Up-case Table system files sit
	// ahead of it), so we scan for it.
	bf := fs2.(*exfatFS)
	rootOff := bf.info.RootDirOffset(bf.partOffset)
	buf := make([]byte, bf.info.ClusterSize())
	if _, err := bf.f.ReadAt(buf, rootOff); err != nil {
		t.Fatalf("read root cluster: %v", err)
	}
	found := false
	for offset := 0; offset+dirEntrySize <= len(buf); offset += dirEntrySize {
		switch buf[offset] {
		case exfatEntryVolumeLabel:
			t.Errorf("root entry at offset %d type = 0x%02x, want 0x%02x (not-in-use)", offset, buf[offset], exfatEntryVolumeLabelUnused)
			found = true
		case exfatEntryVolumeLabelUnused:
			found = true
		}
		if found {
			break
		}
		if buf[offset] == exfatEntryEnd {
			break
		}
	}
	if !found {
		t.Errorf("no Volume Label slot found in root directory after clearing")
	}
}

func TestExfatSetLabel_UTF16Roundtrip(t *testing.T) {
	// Non-ASCII characters that need real UTF-16LE encoding.
	const lbl = "café-2024" // 9 code units in UTF-16, well within MaxLabelLen.
	fs, img := openFreshExfat(t)
	if err := fs.SetLabel(lbl); err != nil {
		t.Fatalf("SetLabel: %v", err)
	}
	fs.Close()
	fs2, err := Open(img, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs2.Close()
	if got := fs2.(*exfatFS).Label(); got != lbl {
		t.Errorf("after UTF-16 roundtrip Label() = %q, want %q", got, lbl)
	}
}

func TestExfatSetLabel_LabelerInterface(t *testing.T) {
	fs, _ := openFreshExfat(t)
	defer fs.Close()
	var f filesystem.Filesystem = fs
	if _, ok := f.(filesystem.Labeller); !ok {
		t.Error("exfatFS does not satisfy filesystem.Labeller")
	}
}
