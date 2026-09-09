package filesystem_exfat

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

// A reader and a path must give the same filesystem, because they are the same
// image: whatever a caller has in its hands, the driver reads the same bytes.
func TestOpenReaderSeesWhatOpenSees(t *testing.T) {
	img := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(img, 32<<20, FormatConfig{Label: "READER"})
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.Repeat([]byte("exfat through a reader "), 500)
	if err := fs.WriteFile("/hello.txt", body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fs.MkDir("/sub", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := fs.Close(); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(img)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	through, err := OpenReader(f, info.Size())
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer through.Close()

	got, err := through.ReadFile("/hello.txt")
	if err != nil || !bytes.Equal(got, body) {
		t.Errorf("read %d bytes of %d through the reader (%v)", len(got), len(body), err)
	}
	entries, err := through.ListDir("/")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 2 {
		t.Errorf("the root lists %v, want hello.txt and sub", names)
	}
	if st, err := through.Stat("/hello.txt"); err != nil || st.Size() != uint64(len(body)) {
		t.Errorf("stat through the reader = %v, %v", st, err)
	}
}

// A reader that cannot be written gives a filesystem that refuses writes, and
// says which kind of refusal it is -- rather than accepting one and losing it.
func TestOpenReaderRefusesWritesItCannotDo(t *testing.T) {
	img := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(img, 32<<20, FormatConfig{Label: "RO"})
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile("/there.txt", []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs.Close()

	raw, err := os.ReadFile(img)
	if err != nil {
		t.Fatal(err)
	}
	// bytes.Reader reads and does not write, which is the case that matters:
	// an image in memory, or a section of a larger file.
	ro, err := OpenReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if got, err := ro.ReadFile("/there.txt"); err != nil || string(got) != "hello" {
		t.Errorf("reading through a read-only reader: %q, %v", got, err)
	}
	if err := ro.WriteFile("/new.txt", []byte("nope"), 0o644); !errors.Is(err, ErrReadOnlyReader) {
		t.Errorf("writing through a read-only reader = %v, want ErrReadOnlyReader", err)
	}

	// And one that CAN be written takes the write.
	rw, err := os.OpenFile(img, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rw.Close()
	info, err := rw.Stat()
	if err != nil {
		t.Fatal(err)
	}
	w, err := OpenReader(rw, info.Size())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.WriteFile("/written.txt", []byte("through a writer"), 0o644); err != nil {
		t.Errorf("writing through a writable reader: %v", err)
	}
	if got, err := w.ReadFile("/written.txt"); err != nil || string(got) != "through a writer" {
		t.Errorf("reading it back: %q, %v", got, err)
	}
	w.Close()
}

// Close must not close what the caller opened: the reader is the caller's, and
// a driver that closes it takes something that was not given.
func TestOpenReaderDoesNotCloseTheCallersReader(t *testing.T) {
	img := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(img, 32<<20, FormatConfig{})
	if err != nil {
		t.Fatal(err)
	}
	fs.Close()
	f, err := os.Open(img)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, _ := f.Stat()
	through, err := OpenReader(f, info.Size())
	if err != nil {
		t.Fatal(err)
	}
	if err := through.Close(); err != nil {
		t.Fatal(err)
	}
	// Still usable, because it was never closed.
	if _, err := f.ReadAt(make([]byte, 512), 0); err != nil {
		t.Errorf("the caller's reader was closed underneath it: %v", err)
	}
}

// Nothing to open is a refusal with a reason.
func TestOpenReaderRefusals(t *testing.T) {
	if _, err := OpenReader(nil, 100); err == nil {
		t.Error("OpenReader accepted no reader at all")
	}
	if _, err := OpenReader(bytes.NewReader(nil), 0); err == nil {
		t.Error("OpenReader accepted a size of zero")
	}
	// Something that is not a exFAT image is refused by the same parser Open
	// uses, not by a second opinion.
	if _, err := OpenReader(bytes.NewReader(make([]byte, 4096)), 4096); err == nil {
		t.Error("OpenReader accepted an image with no boot sector in it")
	}
}

var _ filesystem.Filesystem = (*exfatFS)(nil)
