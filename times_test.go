package filesystem_exfat

import (
	"compress/gzip"
	"errors"
	"io"
	iofs "io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

// timesFixture is an exFAT image written by Apple's reference implementation:
// newfs_exfat on a raw disk image attached with hdiutil, mounted by diskutil,
// populated with files whose times were set by os.Chtimes to the instants below.
// Regenerate on macOS with EXFAT_REGEN_TIMES=1 (see TestTimesRegenerateFixture).
// No Linux exfatprogs tool can set times, so CI uses the committed image.
const timesFixture = "testdata/times-exfat.img.gz"

var timesSources = []struct {
	name  string
	mtime time.Time
	atime time.Time
}{
	{"EVEN.TXT", time.Date(2024, 5, 17, 13, 45, 30, 0, time.UTC), time.Date(2024, 5, 18, 8, 0, 10, 0, time.UTC)},
	{"ODD.TXT", time.Date(2021, 12, 31, 23, 59, 59, 0, time.UTC), time.Date(2021, 12, 31, 23, 59, 59, 0, time.UTC)},
	{"MS.TXT", time.Date(2023, 3, 4, 5, 6, 7, 340_000_000, time.UTC), time.Date(2023, 3, 4, 5, 6, 8, 0, time.UTC)},
}

// TestTimesRegenerateFixture rebuilds the fixture with the macOS tools.
func TestTimesRegenerateFixture(t *testing.T) {
	if os.Getenv("EXFAT_REGEN_TIMES") != "1" {
		t.Skip("set EXFAT_REGEN_TIMES=1 (macOS) to rebuild " + timesFixture)
	}
	dir := t.TempDir()
	img := filepath.Join(dir, "t.img")
	if err := os.WriteFile(img, make([]byte, 0), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(img, 16<<20); err != nil {
		t.Fatal(err)
	}
	run := func(name string, args ...string) string {
		t.Helper()
		cmd := exec.Command(name, args...)
		var stderr strings.Builder
		cmd.Stderr = &stderr // hdiutil warns on stderr; stdout carries the device
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
		return string(out)
	}
	disk := strings.TrimSpace(run("hdiutil", "attach", "-imagekey", "diskimage-class=CRawDiskImage", "-nomount", img))
	detach := func() { _ = exec.Command("hdiutil", "detach", disk).Run() }
	defer detach()
	run("newfs_exfat", "-v", "TIMES", disk)
	run("diskutil", "mount", disk)
	mnt := "/Volumes/TIMES"
	for _, s := range timesSources {
		p := filepath.Join(mnt, s.name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, s.atime, s.mtime); err != nil {
			t.Fatal(err)
		}
	}
	run("diskutil", "unmount", mnt)
	detach()
	in, err := os.Open(img)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(timesFixture)
	if err != nil {
		t.Fatal(err)
	}
	zw, _ := gzip.NewWriterLevel(out, gzip.BestCompression)
	if _, err := io.Copy(zw, in); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	out.Close()
}

func openTimesFixture(t *testing.T) *exfatFS {
	t.Helper()
	gz, err := os.Open(timesFixture)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	zr, err := gzip.NewReader(gz)
	if err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(t.TempDir(), "t.img")
	out, err := os.Create(img)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, zr); err != nil {
		t.Fatal(err)
	}
	out.Close()
	fs, err := Open(img, -1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fs.Close() })
	return fs.(*exfatFS)
}

// TestTimesReferenceImage checks the decoder against entries the reference
// implementation wrote for known source times.
func TestTimesReferenceImage(t *testing.T) {
	fs := openTimesFixture(t)
	for _, s := range timesSources {
		ft, err := fs.Times("/" + s.name)
		if err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		t.Logf("%s: %+v", s.name, ft)
		m, ok := ft.Mtime.Get()
		if !ok {
			t.Fatalf("%s: mtime not recorded", s.name)
		}
		if ft.Mtime.Resolution() != 10*time.Millisecond || ft.Btime.Resolution() != 10*time.Millisecond || ft.Atime.Resolution() != 2*time.Second {
			t.Errorf("%s: resolutions %v %v %v", s.name, ft.Mtime.Resolution(), ft.Btime.Resolution(), ft.Atime.Resolution())
		}
		if d := m.Sub(s.mtime).Abs(); d > 10*time.Millisecond {
			t.Errorf("%s: mtime = %v, want %v", s.name, m, s.mtime)
		}
		a, ok := ft.Atime.Get()
		if !ok || a.Sub(s.atime).Abs() > 2*time.Second {
			t.Errorf("%s: atime = %v, want %v", s.name, a, s.atime)
		}
		if ft.Btime.Recorded() == false {
			t.Errorf("%s: btime missing", s.name)
		}
		if ft.Ctime.Recorded() {
			t.Errorf("%s: exFAT has no ctime", s.name)
		}
	}
}

func TestTimesRootAndMissing(t *testing.T) {
	fs := openTimesFixture(t)
	ft, err := fs.Times("/")
	if err != nil || ft != (filesystem.FileTimes{}) {
		t.Fatalf("root: %+v %v", ft, err)
	}
	if _, err := fs.Times("/NOPE.TXT"); !errors.Is(err, iofs.ErrNotExist) {
		t.Fatalf("err = %v", err)
	}
}

// timesBytes builds the 17 bytes at offsets 8..24 of a File entry.
func timesBytes(cts, mts, ats uint32, cinc, minc, coff, moff, aoff uint8) []byte {
	b := make([]byte, 17)
	for i, v := range []uint32{cts, mts, ats} {
		b[i*4], b[i*4+1], b[i*4+2], b[i*4+3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
	}
	b[12], b[13], b[14], b[15], b[16] = cinc, minc, coff, moff, aoff
	return b
}

func ts(y, mo, d, h, mi, s int) uint32 {
	return uint32(y-1980)<<25 | uint32(mo)<<21 | uint32(d)<<16 | uint32(h)<<11 | uint32(mi)<<5 | uint32(s/2)
}

// TestDecodeExfatTimes covers the encoding exactly with hand-built words: the
// bit layout, the increments, the UTC offset byte and every invalid field.
func TestDecodeExfatTimes(t *testing.T) {
	old := time.Local
	time.Local = time.FixedZone("T", 3600)
	t.Cleanup(func() { time.Local = old })

	base := ts(2024, 5, 17, 13, 45, 30)
	// +05:30 = 22 steps; -08:00 = -32 = 0x60; offsets with OffsetValid set.
	ft := decodeExfatTimes(timesBytes(base, base, base, 135, 199, 0x80|22, 0x80|0x60, 0x80|0x7F))
	want := map[string]time.Time{
		"b": time.Date(2024, 5, 17, 13, 45, 31, 350_000_000, time.FixedZone("", 22*900)),
		"m": time.Date(2024, 5, 17, 13, 45, 31, 990_000_000, time.FixedZone("", -8*3600)),
		"a": time.Date(2024, 5, 17, 13, 45, 30, 0, time.FixedZone("", -900)),
	}
	for k, got := range map[string]filesystem.Timestamp{"b": ft.Btime, "m": ft.Mtime, "a": ft.Atime} {
		if g, _ := got.Get(); !g.Equal(want[k]) {
			t.Errorf("%s = %v, want %v", k, g, want[k])
		}
	}
	if ft.Btime.Resolution() != 10*time.Millisecond || ft.Mtime.Resolution() != 10*time.Millisecond || ft.Atime.Resolution() != 2*time.Second {
		t.Error("resolutions")
	}
	// OffsetValid clear: current local offset (+01:00 here), and the offset
	// bits are ignored.
	ft = decodeExfatTimes(timesBytes(base, base, base, 0, 0, 0x05, 0, 0))
	if g, _ := ft.Btime.Get(); !g.Equal(time.Date(2024, 5, 17, 13, 45, 30, 0, time.Local)) {
		t.Errorf("invalid offset: %v", g)
	}
	// 0x80 alone is a valid UTC offset of zero.
	if g, _ := decodeExfatTime(base, 0, 0x80, 2*time.Second).Get(); !g.Equal(time.Date(2024, 5, 17, 13, 45, 30, 0, time.UTC)) {
		t.Errorf("utc: %v", g)
	}
	// 0x40 is -16:00, 0x3F is +15:45.
	if g, _ := decodeExfatTime(base, 0, 0x80|0x40, time.Second).Get(); !g.Equal(time.Date(2024, 5, 17, 13, 45, 30, 0, time.FixedZone("", -16*3600))) {
		t.Errorf("-16h: %v", g)
	}
	if g, _ := decodeExfatTime(base, 0, 0x80|0x3F, time.Second).Get(); !g.Equal(time.Date(2024, 5, 17, 13, 45, 30, 0, time.FixedZone("", 15*3600+45*60))) {
		t.Errorf("+15:45: %v", g)
	}

	absent := []struct {
		name string
		ts   uint32
		inc  uint8
	}{
		{"all zero", 0, 0},
		{"month 0", ts(2024, 0, 17, 1, 1, 0), 0},
		{"month 13", ts(2024, 13, 17, 1, 1, 0), 0},
		{"day 0", ts(2024, 5, 0, 1, 1, 0), 0},
		{"31 April", ts(2024, 4, 31, 1, 1, 0), 0},
		{"29 Feb 2023", ts(2023, 2, 29, 1, 1, 0), 0},
		{"hour 24", ts(2024, 5, 17, 0, 0, 0) | 24<<11, 0},
		{"minute 60", ts(2024, 5, 17, 0, 0, 0) | 60<<5, 0},
		{"doubleseconds 30", ts(2024, 5, 17, 0, 0, 0) | 30, 0},
		{"increment 200", base, 200},
	}
	for _, c := range absent {
		if got := decodeExfatTime(c.ts, c.inc, 0x80, 10*time.Millisecond); got.Recorded() {
			t.Errorf("%s decoded to %v", c.name, got)
		}
	}
	if !decodeExfatTime(ts(2024, 2, 29, 0, 0, 0), 0, 0x80, 0).Recorded() {
		t.Error("29 Feb 2024 is valid")
	}
	if !decodeExfatTime(ts(2107, 12, 31, 23, 59, 58), 199, 0x80, 0).Recorded() {
		t.Error("year 2107 is valid")
	}
}

func FuzzDecodeExfatTimes(f *testing.F) {
	f.Add(make([]byte, 17))
	f.Add(timesBytes(ts(2024, 5, 17, 13, 45, 30), ts(2024, 5, 17, 13, 45, 30), ts(2024, 5, 17, 13, 45, 30), 135, 199, 0x80|22, 0x80|0x60, 0x80|0x7F))
	f.Add(timesBytes(0xFFFFFFFF, 0xFFFFFFFF, 0xFFFFFFFF, 255, 255, 0xFF, 0xFF, 0xFF))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) < 17 {
			return
		}
		ft := decodeExfatTimes(b[:17])
		for _, x := range []filesystem.Timestamp{ft.Atime, ft.Mtime, ft.Btime} {
			if m, ok := x.Get(); ok {
				if m.IsZero() || m.Year() < 1979 || m.Year() > 2108 || x.Resolution() <= 0 {
					t.Fatalf("recorded %v res %v from %x", m, x.Resolution(), b[:17])
				}
			}
		}
		if ft.Ctime.Recorded() {
			t.Fatal("ctime")
		}
	})
}
