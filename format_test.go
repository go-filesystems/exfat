package filesystem_exfat

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

const exfatTestSize = 4 * 1024 * 1024 // 4 MiB

var errExfatBoom = errors.New("exfat format injected error")

// ── Validation errors ─────────────────────────────────────────────────────

func TestExFmt_NotMultipleOfClusterSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.img")
	if _, err := Format(path, 4097, FormatConfig{}); err == nil {
		t.Error("expected error: size not a multiple of cluster size")
	}
}

func TestExFmt_TooSmall(t *testing.T) {
	// 16384 = 4×4096; cluster heap starts at sector 32 = 16384 bytes,
	// so clusterCount = (32-32)/8 = 0 → too small.
	path := filepath.Join(t.TempDir(), "tiny.img")
	if _, err := Format(path, 16384, FormatConfig{}); err == nil {
		t.Error("expected error: size too small")
	}
}

// ── Happy-path basics ─────────────────────────────────────────────────────

func TestExFmt_CreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new.img")
	fs, err := Format(path, exfatTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	fs.Close()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("image file not created: %v", err)
	}
}

func TestExFmt_FileSizePreserved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, exfatTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	fs.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != exfatTestSize {
		t.Errorf("size = %d, want %d", info.Size(), exfatTestSize)
	}
}

func TestExFmt_TruncatesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.img")
	if err := os.WriteFile(path, make([]byte, 512*1024), 0o600); err != nil {
		t.Fatal(err)
	}
	fs, err := Format(path, exfatTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	fs.Close()
}

func TestExFmt_StatRoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, exfatTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	st, err := fs.Stat("/")
	if err != nil {
		t.Fatalf("Stat /: %v", err)
	}
	if st.Mode()&0xF000 != 0x4000 {
		t.Errorf("root mode 0x%04X is not a directory", st.Mode())
	}
}

func TestExFmt_ListDirRoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, exfatTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	if _, err := fs.ListDir("/"); err != nil {
		t.Fatalf("ListDir /: %v", err)
	}
}

func TestExFmt_WriteReadRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, exfatTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	const data = "hello from exFAT Format\n"
	if err := fs.WriteFile("/hello.txt", []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := fs.ReadFile("/hello.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != data {
		t.Errorf("got %q, want %q", got, data)
	}
}

func TestExFmt_ZeroSerialNumberFallback(t *testing.T) {
	old := formatRandUint32
	formatRandUint32 = func() uint32 { return 0 }
	t.Cleanup(func() { formatRandUint32 = old })
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, exfatTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format with zero rand serial: %v", err)
	}
	fs.Close()
}

func TestExFmt_ReOpenAndWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	{
		fs, err := Format(path, exfatTestSize, FormatConfig{})
		if err != nil {
			t.Fatalf("Format: %v", err)
		}
		if err := fs.WriteFile("/data.bin", []byte("original"), 0o600); err != nil {
			fs.Close()
			t.Fatalf("WriteFile: %v", err)
		}
		fs.Close()
	}
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()
	got, err := fs.ReadFile("/data.bin")
	if err != nil {
		t.Fatalf("ReadFile after re-open: %v", err)
	}
	if string(got) != "original" {
		t.Errorf("got %q, want %q", got, "original")
	}
}

// ── Error injection ───────────────────────────────────────────────────────

type exfatCountingFile struct {
	inner     formatFile
	writeCall int
	failAt    int
}

func (f *exfatCountingFile) WriteAt(p []byte, off int64) (int, error) {
	f.writeCall++
	if f.writeCall == f.failAt {
		return 0, errExfatBoom
	}
	return f.inner.WriteAt(p, off)
}
func (f *exfatCountingFile) Truncate(n int64) error { return f.inner.Truncate(n) }
func (f *exfatCountingFile) Close() error           { return f.inner.Close() }

type exfatTruncFailFile struct{}

func (f *exfatTruncFailFile) WriteAt([]byte, int64) (int, error) { return 0, nil }
func (f *exfatTruncFailFile) Truncate(int64) error               { return errExfatBoom }
func (f *exfatTruncFailFile) Close() error                       { return nil }

type exfatCloseFailFile struct{ inner formatFile }

func (f *exfatCloseFailFile) WriteAt(p []byte, off int64) (int, error) {
	return f.inner.WriteAt(p, off)
}
func (f *exfatCloseFailFile) Truncate(n int64) error { return f.inner.Truncate(n) }
func (f *exfatCloseFailFile) Close() error           { return errExfatBoom }

func injectExfatCounting(t *testing.T, failAt int) {
	t.Helper()
	old := formatOpenFile
	formatOpenFile = func(path string) (formatFile, error) {
		inner, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return nil, err
		}
		return &exfatCountingFile{inner: inner, failAt: failAt}, nil
	}
	t.Cleanup(func() { formatOpenFile = old })
}

func exfatExpectBoom(t *testing.T) {
	t.Helper()
	if _, err := Format(filepath.Join(t.TempDir(), "x.img"), exfatTestSize, FormatConfig{}); !errors.Is(err, errExfatBoom) {
		t.Fatalf("expected errExfatBoom, got %v", err)
	}
}

func TestExFmt_OpenFileFails(t *testing.T) {
	old := formatOpenFile
	formatOpenFile = func(string) (formatFile, error) { return nil, errExfatBoom }
	t.Cleanup(func() { formatOpenFile = old })
	exfatExpectBoom(t)
}

func TestExFmt_TruncateFails(t *testing.T) {
	old := formatOpenFile
	formatOpenFile = func(string) (formatFile, error) { return &exfatTruncFailFile{}, nil }
	t.Cleanup(func() { formatOpenFile = old })
	exfatExpectBoom(t)
}

func TestExFmt_WriteMainBootFails(t *testing.T)   { injectExfatCounting(t, 1); exfatExpectBoom(t) }
func TestExFmt_WriteBackupBootFails(t *testing.T) { injectExfatCounting(t, 2); exfatExpectBoom(t) }
func TestExFmt_WriteFATFails(t *testing.T)        { injectExfatCounting(t, 3); exfatExpectBoom(t) }

func TestExFmt_CloseFails(t *testing.T) {
	old := formatOpenFile
	formatOpenFile = func(path string) (formatFile, error) {
		inner, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return nil, err
		}
		return &exfatCloseFailFile{inner: inner}, nil
	}
	t.Cleanup(func() { formatOpenFile = old })
	exfatExpectBoom(t)
}

func TestExFmt_OpenFSFails(t *testing.T) {
	old := formatOpenFS
	formatOpenFS = func(string, int) (filesystem.Filesystem, error) { return nil, errExfatBoom }
	t.Cleanup(func() { formatOpenFS = old })
	exfatExpectBoom(t)
}
