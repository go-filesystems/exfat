package filesystem_exfat

import (
	"bytes"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

// truncateImageSize is large enough to hold a multi-cluster file plus the
// system metadata produced by Format.
const truncateImageSize = 4 * 1024 * 1024

func TestTruncate(t *testing.T) {
	fs, path := freshFormattedFS(t, truncateImageSize)

	// Confirm the optional capability is detectable through the interface.
	var ifc filesystem.Filesystem = fs
	tr, ok := ifc.(filesystem.Truncater)
	if !ok {
		t.Fatal("exfatFS does not satisfy filesystem.Truncater")
	}

	// Seed a multi-cluster file with a recognisable pattern.
	orig := make([]byte, 5000)
	for i := range orig {
		orig[i] = byte(i%251 + 1) // never zero, so zero-fill is distinguishable
	}
	if err := fs.WriteFile("/data.bin", orig, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// ── Shrink ────────────────────────────────────────────────────────────
	if err := tr.Truncate("/data.bin", 1000); err != nil {
		t.Fatalf("Truncate(shrink): %v", err)
	}
	got, err := fs.ReadFile("/data.bin")
	if err != nil {
		t.Fatalf("ReadFile after shrink: %v", err)
	}
	if len(got) != 1000 {
		t.Fatalf("shrink length = %d, want 1000", len(got))
	}
	if !bytes.Equal(got, orig[:1000]) {
		t.Fatal("shrink prefix mismatch")
	}
	if st, err := fs.Stat("/data.bin"); err != nil {
		t.Fatalf("Stat after shrink: %v", err)
	} else if st.Size() != 1000 {
		t.Fatalf("Stat size after shrink = %d, want 1000", st.Size())
	}

	// ── Grow ──────────────────────────────────────────────────────────────
	if err := tr.Truncate("/data.bin", 9000); err != nil {
		t.Fatalf("Truncate(grow): %v", err)
	}
	got, err = fs.ReadFile("/data.bin")
	if err != nil {
		t.Fatalf("ReadFile after grow: %v", err)
	}
	if len(got) != 9000 {
		t.Fatalf("grow length = %d, want 9000", len(got))
	}
	if !bytes.Equal(got[:1000], orig[:1000]) {
		t.Fatal("grow prefix mismatch")
	}
	if !bytes.Equal(got[1000:], make([]byte, 8000)) {
		t.Fatal("grow extended region is not zero-filled")
	}

	// ── Survives close / reopen ────────────────────────────────────────────
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	re := reopenAs(t, path)
	got, err = re.ReadFile("/data.bin")
	if err != nil {
		t.Fatalf("ReadFile after reopen: %v", err)
	}
	if len(got) != 9000 {
		t.Fatalf("reopen length = %d, want 9000", len(got))
	}
	if !bytes.Equal(got[:1000], orig[:1000]) || !bytes.Equal(got[1000:], make([]byte, 8000)) {
		t.Fatal("reopen content mismatch")
	}
}

func TestTruncateToZero(t *testing.T) {
	fs, _ := freshFormattedFS(t, truncateImageSize)
	if err := fs.WriteFile("/z.bin", []byte("payload"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := fs.Truncate("/z.bin", 0); err != nil {
		t.Fatalf("Truncate(0): %v", err)
	}
	got, err := fs.ReadFile("/z.bin")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("length after truncate-to-zero = %d, want 0", len(got))
	}
	// Growing a zero-length file back up must zero-fill cleanly.
	if err := fs.Truncate("/z.bin", 100); err != nil {
		t.Fatalf("Truncate(grow from zero): %v", err)
	}
	got, err = fs.ReadFile("/z.bin")
	if err != nil {
		t.Fatalf("ReadFile after grow from zero: %v", err)
	}
	if len(got) != 100 || !bytes.Equal(got, make([]byte, 100)) {
		t.Fatal("grow from zero is not zero-filled / wrong length")
	}
}

func TestTruncateRejectsDirectory(t *testing.T) {
	fs, _ := freshFormattedFS(t, truncateImageSize)
	if err := fs.MkDir("/adir", 0o755); err != nil {
		t.Fatalf("MkDir: %v", err)
	}
	if err := fs.Truncate("/adir", 0); err == nil {
		t.Fatal("Truncate on directory error = nil, want error")
	}
	if err := fs.Truncate("/", 0); err == nil {
		t.Fatal("Truncate on root error = nil, want error")
	}
}

func TestTruncateMissingFile(t *testing.T) {
	fs, _ := freshFormattedFS(t, truncateImageSize)
	if err := fs.Truncate("/nope.bin", 10); err == nil {
		t.Fatal("Truncate on missing file error = nil, want error")
	}
}
