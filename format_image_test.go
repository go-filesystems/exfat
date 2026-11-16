package filesystem_exfat

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// memImage is an in-memory io.ReaderAt + io.WriterAt of fixed size, standing in
// for an encrypted volume or any other target that is not a file on disk.
type memImage struct {
	b []byte
	// failAfter, when >= 0, makes the (failAfter+1)-th WriteAt fail.
	failAfter int
	// dropWrites makes WriteAt report success without storing anything.
	dropWrites bool
	writes     int
}

func newMemImage(n int) *memImage { return &memImage{b: make([]byte, n), failAfter: -1} }

func (m *memImage) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(m.b)) {
		return 0, io.EOF
	}
	n := copy(p, m.b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (m *memImage) WriteAt(p []byte, off int64) (int, error) {
	if m.failAfter >= 0 && m.writes >= m.failAfter {
		return 0, errExfatBoom
	}
	m.writes++
	if m.dropWrites {
		return len(p), nil
	}
	if off+int64(len(p)) > int64(len(m.b)) {
		return 0, io.ErrShortWrite
	}
	return copy(m.b[off:], p), nil
}

// formatToFile runs Format(path) and returns the bytes of the file it wrote.
func formatToFile(t *testing.T, size int64, cfg FormatConfig) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ref.img")
	fs, err := Format(path, size, cfg)
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFormatImage_ByteIdenticalToFormat(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int64
		cfg  FormatConfig
	}{
		{"4MiB", 4 << 20, FormatConfig{VolumeSerialNumber: 0x1234ABCD}},
		{"4MiB-label", 4 << 20, FormatConfig{Label: "THECA", VolumeSerialNumber: 7}},
		{"64MiB", 64 << 20, FormatConfig{Label: "bench", VolumeSerialNumber: 0xCAFEF00D}},
		{"256MiB-bigger-cluster", 256 << 20, FormatConfig{VolumeSerialNumber: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := formatToFile(t, tc.size, tc.cfg)
			img := newMemImage(int(tc.size))
			fs, err := FormatImage(img, tc.size, tc.cfg)
			if err != nil {
				t.Fatalf("FormatImage: %v", err)
			}
			if err := fs.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if !bytes.Equal(img.b, want) {
				for i := range want {
					if img.b[i] != want[i] {
						t.Fatalf("image differs from Format(path) at byte %d: %#x vs %#x", i, img.b[i], want[i])
					}
				}
			}
		})
	}
}

// A random serial is drawn the same way by both paths.
func TestFormatImage_RandomSerial(t *testing.T) {
	old := formatRandUint32
	formatRandUint32 = func() uint32 { return 0 }
	t.Cleanup(func() { formatRandUint32 = old })
	want := formatToFile(t, exfatTestSize, FormatConfig{})
	img := newMemImage(exfatTestSize)
	fs, err := FormatImage(img, exfatTestSize, FormatConfig{})
	if err != nil {
		t.Fatal(err)
	}
	fs.Close()
	if !bytes.Equal(img.b, want) {
		t.Fatal("image differs from Format(path) with the zero-serial fallback")
	}
}

// A buffer that held something else must not leak into the new volume: the
// metadata region ends up as Format's, byte for byte.
func TestFormatImage_ReusedBuffer(t *testing.T) {
	cfg := FormatConfig{Label: "reuse", VolumeSerialNumber: 99}
	want := formatToFile(t, exfatTestSize, cfg)
	img := newMemImage(exfatTestSize)
	for i := range img.b {
		img.b[i] = 0xA5
	}
	fs, err := FormatImage(img, exfatTestSize, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	l, err := formatLayout(exfatTestSize)
	if err != nil {
		t.Fatal(err)
	}
	end := int(l.clusterHeapOffset)*512 + 3*(512<<l.sectorShift) // through the root cluster
	if !bytes.Equal(img.b[:end], want[:end]) {
		t.Fatal("metadata region of a reused buffer differs from Format(path)")
	}
	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("root has %d entries on a fresh volume", len(entries))
	}
}

func TestFormatImage_WriteReadRenameDelete(t *testing.T) {
	img := newMemImage(exfatTestSize)
	fs, err := FormatImage(img, exfatTestSize, FormatConfig{Label: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	const data = "hello from FormatImage\n"
	if err := fs.WriteFile("/a.txt", []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := fs.ReadFile("/a.txt")
	if err != nil || string(got) != data {
		t.Fatalf("ReadFile = %q, %v", got, err)
	}
	if err := fs.Rename("/a.txt", "/b.txt"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, err := fs.ReadFile("/a.txt"); err == nil {
		t.Fatal("old name still readable after Rename")
	}
	if got, err := fs.ReadFile("/b.txt"); err != nil || string(got) != data {
		t.Fatalf("ReadFile renamed = %q, %v", got, err)
	}
	if err := fs.DeleteFile("/b.txt"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if _, err := fs.ReadFile("/b.txt"); err == nil {
		t.Fatal("file readable after DeleteFile")
	}
	// What was written reached rw, and a second open over rw sees the volume.
	if err := fs.WriteFile("/c.txt", []byte("persisted"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs.Close()
	again, err := OpenReader(img, exfatTestSize)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer again.Close()
	if got, err := again.ReadFile("/c.txt"); err != nil || string(got) != "persisted" {
		t.Fatalf("re-open ReadFile = %q, %v", got, err)
	}
}

// FormatImage refuses what Format refuses, with the same message, and writes
// nothing when it does.
func TestFormatImage_Refusals(t *testing.T) {
	for _, size := range []int64{0, -4096, 1, 512, 4097, 8192 + 100, 16384, 20 * 4096} {
		img := newMemImage(1 << 20)
		_, errImg := FormatImage(img, size, FormatConfig{})
		_, errPath := Format(filepath.Join(t.TempDir(), "x.img"), size, FormatConfig{})
		if (errImg == nil) != (errPath == nil) {
			t.Fatalf("size %d: FormatImage err=%v, Format err=%v", size, errImg, errPath)
		}
		if errImg != nil {
			if errImg.Error() != errPath.Error() {
				t.Errorf("size %d: messages differ: %q vs %q", size, errImg, errPath)
			}
			if !bytes.Equal(img.b, make([]byte, len(img.b))) {
				t.Errorf("size %d: refused but wrote to the image", size)
			}
		}
	}
	if _, err := FormatImage(nil, exfatTestSize, FormatConfig{}); err == nil {
		t.Error("nil image accepted")
	}
	// Sizes that are a multiple of 4096 but not of the cluster size are
	// accepted, as Format accepts them.
	for _, size := range []int64{8 << 20, 4<<20 + 4096} {
		if _, err := FormatImage(newMemImage(int(size)), size, FormatConfig{}); err != nil {
			t.Errorf("size %d: %v", size, err)
		}
	}
}

// An image smaller than sizeBytes is a write error, never a silent truncation.
func TestFormatImage_ImageTooShort(t *testing.T) {
	if _, err := FormatImage(newMemImage(exfatTestSize/2), exfatTestSize, FormatConfig{}); err == nil {
		t.Fatal("expected an error for an image shorter than sizeBytes")
	}
}

// emptyRead reads nothing and reports no error, which io.ReaderAt permits only
// for a zero-length read: FormatImage must not take it for a long enough image.
type emptyRead struct{ *memImage }

func (emptyRead) ReadAt([]byte, int64) (int, error) { return 0, nil }

func TestFormatImage_ProbeWithoutError(t *testing.T) {
	_, err := FormatImage(emptyRead{newMemImage(exfatTestSize)}, exfatTestSize, FormatConfig{})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestFormatImage_WriteFailures(t *testing.T) {
	// Each position fails in turn: the clear, then every structure written.
	for n := 0; n < 40; n++ {
		img := newMemImage(exfatTestSize)
		img.failAfter = n
		fs, err := FormatImage(img, exfatTestSize, FormatConfig{VolumeSerialNumber: 5})
		if err == nil {
			fs.Close()
			if n < 20 {
				t.Fatalf("failAfter=%d: no error", n)
			}
			continue
		}
		if !errors.Is(err, errExfatBoom) {
			t.Fatalf("failAfter=%d: error does not wrap the cause: %v", n, err)
		}
	}
}

// A label SetLabel refuses is refused by both paths.
func TestFormatImage_LabelRefused(t *testing.T) {
	cfg := FormatConfig{Label: "this label is far too long"}
	if _, err := Format(filepath.Join(t.TempDir(), "l.img"), exfatTestSize, cfg); err == nil {
		t.Fatal("Format accepted the label")
	}
	if _, err := FormatImage(newMemImage(exfatTestSize), exfatTestSize, cfg); err == nil {
		t.Fatal("FormatImage accepted the label")
	}
}

func TestFormatImage_NothingStored(t *testing.T) {
	img := newMemImage(exfatTestSize)
	img.dropWrites = true
	if _, err := FormatImage(img, exfatTestSize, FormatConfig{}); err == nil {
		t.Fatal("expected OpenReader to refuse an image that holds no volume")
	}
}

func BenchmarkFormatImage64MiB(b *testing.B) {
	const size = 64 << 20
	img := newMemImage(size)
	b.SetBytes(size)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fs, err := FormatImage(img, size, FormatConfig{VolumeSerialNumber: 1})
		if err != nil {
			b.Fatal(err)
		}
		fs.Close()
	}
}

func BenchmarkFormatPath64MiB(b *testing.B) {
	const size = 64 << 20
	dir := b.TempDir()
	path := filepath.Join(dir, "p.img")
	b.SetBytes(size)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fs, err := Format(path, size, FormatConfig{VolumeSerialNumber: 1})
		if err != nil {
			b.Fatal(err)
		}
		fs.Close()
	}
}
