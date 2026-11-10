package filesystem_exfat

import (
	"bytes"
	"encoding/binary"
	"errors"
	iofs "io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

type chtimesFS interface {
	filesystem.Filesystem
	filesystem.TimesReader
	Chtimes(path string, atime, mtime time.Time) error
}

func setLocal(t testing.TB, loc *time.Location) {
	old := time.Local
	time.Local = loc
	t.Cleanup(func() { time.Local = old })
}

func formatChtimes(t *testing.T) (chtimesFS, string) {
	t.Helper()
	img := filepath.Join(t.TempDir(), "ct.img")
	fs, err := Format(img, 32<<20, FormatConfig{Label: "CT"})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"/A.TXT", "/B.TXT"} {
		if err := fs.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := fs.MkDir("/SUB", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile("/SUB/C.TXT", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return fs.(chtimesFS), img
}

// TestMetadataSetter: the driver satisfies the whole interface (the assertion
// smb's SET_INFO path, go-fileshare's lock wrapper, NFS and SFTP make), and
// Chmod and Chown refuse with a typed error and change nothing.
func TestMetadataSetter(t *testing.T) {
	fs, _ := formatChtimes(t)
	defer fs.Close()
	var f filesystem.Filesystem = fs
	ms, ok := f.(filesystem.MetadataSetter)
	if !ok {
		t.Fatal("exfat is not a filesystem.MetadataSetter")
	}
	before, _ := fs.Times("/A.TXT")
	for name, err := range map[string]error{
		"Chmod": ms.Chmod("/A.TXT", 0o600),
		"Chown": ms.Chown("/A.TXT", 1, 2),
	} {
		if !errors.Is(err, ErrNotSupported) || !errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("%s = %v, want ErrNotSupported (wrapping errors.ErrUnsupported)", name, err)
		}
	}
	if after, _ := fs.Times("/A.TXT"); after != before {
		t.Error("a refused Chmod/Chown changed the entry")
	}
	now := time.Now()
	if err := ms.Chtimes("/A.TXT", now, now); err != nil {
		t.Fatalf("Chtimes through the interface: %v", err)
	}
}

func timesOf(t *testing.T, fs chtimesFS, p string) (a, m time.Time) {
	t.Helper()
	ft, err := fs.Times(p)
	if err != nil {
		t.Fatalf("Times(%s): %v", p, err)
	}
	a, _ = ft.Atime.Get()
	m, _ = ft.Mtime.Get()
	return a, m
}

// TestChtimesRoundTrip: what Times reports after Chtimes is the instant rounded
// to the format's resolution (mtime down to 10 ms, atime down to 2 s), for a
// file, a nested file and a directory, in several local zones including ones
// the 15-minute offset field cannot hold.
func TestChtimesRoundTrip(t *testing.T) {
	zones := []*time.Location{
		time.UTC,
		time.FixedZone("CET", 3600),
		time.FixedZone("NYC", -5*3600),
		time.FixedZone("IST", 5*3600+1800),
		time.FixedZone("LMT", 3600+30), // not a 15-minute step: written as UTC
		time.FixedZone("far", 17*3600), // beyond +15:45: written as UTC
	}
	for _, zone := range zones {
		setLocal(t, zone)
		fs, img := formatChtimes(t)
		at := time.Date(2024, 5, 17, 13, 45, 31, 999_000_000, zone)
		mt := time.Date(2021, 12, 31, 23, 59, 59, 347_000_000, zone)
		for _, p := range []string{"/A.TXT", "/SUB", "/SUB/C.TXT"} {
			before, _ := fs.Times(p)
			if err := fs.Chtimes(p, at, mt); err != nil {
				t.Fatalf("%s %s: Chtimes: %v", zone, p, err)
			}
			a, m := timesOf(t, fs, p)
			if want := at.Truncate(2 * time.Second); !a.Equal(want) {
				t.Errorf("%s %s: atime %v, want %v", zone, p, a, want)
			}
			if want := mt.Truncate(10 * time.Millisecond); !m.Equal(want) {
				t.Errorf("%s %s: mtime %v, want %v", zone, p, m, want)
			}
			if ft, _ := fs.Times(p); ft.Btime != before.Btime {
				t.Errorf("%s %s: create time changed", zone, p)
			}
		}
		fs.Close()
		re, err := Open(img, -1)
		if err != nil {
			t.Fatal(err)
		}
		if _, m := timesOf(t, re.(chtimesFS), "/SUB/C.TXT"); !m.Equal(mt.Truncate(10 * time.Millisecond)) {
			t.Errorf("%s: after reopen %v", zone, m)
		}
		re.Close()
	}
}

// TestChtimesEntrySet checks the bytes: the zone fields, OffsetValid, and the
// SetChecksum of the whole entry set recomputed after the edit.
func TestChtimesEntrySet(t *testing.T) {
	setLocal(t, time.FixedZone("IST", 5*3600+1800))
	fs, _ := formatChtimes(t)
	defer fs.Close()
	x := fs.(*exfatFS)
	mt := time.Date(2024, 5, 17, 13, 45, 31, 350_000_000, time.Local)
	at := time.Date(2024, 5, 18, 8, 0, 10, 0, time.Local)
	if err := x.Chtimes("/A.TXT", at, mt); err != nil {
		t.Fatal(err)
	}
	buf, err := x.readDirBuf(x.info.RootDirectoryCluster)
	if err != nil {
		t.Fatal(err)
	}
	off, n := exfatFindEntry(buf, "A.TXT")
	e := buf[off : off+(n+1)*dirEntrySize]
	le := binary.LittleEndian
	if got := le.Uint32(e[12:]); got != ts(2024, 5, 17, 13, 45, 30) {
		t.Errorf("modify timestamp %#x", got)
	}
	if e[21] != 135 {
		t.Errorf("modify increment %d, want 135 (1 s + 350 ms)", e[21])
	}
	if e[23] != 0x80|22 || e[24] != 0x80|22 {
		t.Errorf("offsets %#x %#x, want OffsetValid | 22 (+05:30)", e[23], e[24])
	}
	if got := le.Uint32(e[16:]); got != ts(2024, 5, 18, 8, 0, 10) {
		t.Errorf("access timestamp %#x", got)
	}
	if want := exfatEntrySetChecksum(e); le.Uint16(e[2:]) != want {
		t.Errorf("SetChecksum %#x, want %#x", le.Uint16(e[2:]), want)
	}
}

func TestChtimesRangeEdges(t *testing.T) {
	setLocal(t, time.UTC)
	fs, _ := formatChtimes(t)
	defer fs.Close()
	for _, tm := range []time.Time{
		time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2107, 12, 31, 23, 59, 59, 999_999_999, time.UTC),
		time.Date(2000, 2, 29, 12, 0, 1, 10_000_000, time.UTC),
	} {
		if err := fs.Chtimes("/A.TXT", tm, tm); err != nil {
			t.Fatalf("%v: %v", tm, err)
		}
		a, m := timesOf(t, fs, "/A.TXT")
		if !m.Equal(tm.Truncate(10*time.Millisecond)) || !a.Equal(tm.Truncate(2*time.Second)) {
			t.Errorf("%v: got atime %v mtime %v", tm, a, m)
		}
	}
	a0, m0 := timesOf(t, fs, "/A.TXT")
	for _, tm := range []time.Time{
		time.Date(1979, 12, 31, 23, 59, 59, 0, time.UTC),
		time.Date(2108, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Unix(1<<40, 0),
	} {
		for _, which := range []string{"atime", "mtime"} {
			good := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
			var err error
			if which == "atime" {
				err = fs.Chtimes("/A.TXT", tm, good)
			} else {
				err = fs.Chtimes("/A.TXT", good, tm)
			}
			var re *TimeRangeError
			if !errors.Is(err, ErrTimeOutOfRange) || !errors.As(err, &re) || re.Field != which || !strings.Contains(err.Error(), which) {
				t.Errorf("%v as %s: err = %v", tm, which, err)
			}
			if a, m := timesOf(t, fs, "/A.TXT"); !a.Equal(a0) || !m.Equal(m0) {
				t.Errorf("%v as %s: a refused call changed the times", tm, which)
			}
		}
	}
	// The range is of the wall clock that is written: 1980-01-01 00:30 at +01:00
	// is 1979-12-31 23:30 UTC but is fine in a +01:00 local zone, and the
	// reverse at the top end.
	setLocal(t, time.FixedZone("CET", 3600))
	if err := fs.Chtimes("/A.TXT", time.Time{}, time.Date(1980, 1, 1, 0, 30, 0, 0, time.Local)); err != nil {
		t.Errorf("local 1980-01-01 00:30: %v", err)
	}
	var re *TimeRangeError
	if err := fs.Chtimes("/A.TXT", time.Time{}, time.Date(2107, 12, 31, 23, 30, 0, 0, time.UTC)); !errors.As(err, &re) {
		t.Errorf("local 2108-01-01 00:30 accepted: %v", err)
	}
}

func TestChtimesZeroLeavesUnchanged(t *testing.T) {
	setLocal(t, time.FixedZone("CET", 3600))
	fs, _ := formatChtimes(t)
	defer fs.Close()
	at := time.Date(2020, 3, 4, 5, 6, 8, 0, time.Local)
	mt := time.Date(2022, 9, 10, 11, 12, 14, 70_000_000, time.Local)
	if err := fs.Chtimes("/A.TXT", at, mt); err != nil {
		t.Fatal(err)
	}
	mt2 := time.Date(2023, 1, 2, 3, 4, 6, 0, time.Local)
	if err := fs.Chtimes("/A.TXT", time.Time{}, mt2); err != nil {
		t.Fatal(err)
	}
	if a, m := timesOf(t, fs, "/A.TXT"); !a.Equal(at) || !m.Equal(mt2) {
		t.Errorf("zero atime: got %v %v", a, m)
	}
	at2 := time.Date(2024, 6, 7, 1, 2, 4, 0, time.Local)
	if err := fs.Chtimes("/A.TXT", at2, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if a, m := timesOf(t, fs, "/A.TXT"); !a.Equal(at2) || !m.Equal(mt2) {
		t.Errorf("zero mtime: got %v %v", a, m)
	}
	if err := fs.Chtimes("/A.TXT", time.Time{}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if a, m := timesOf(t, fs, "/A.TXT"); !a.Equal(at2) || !m.Equal(mt2) {
		t.Errorf("both zero: got %v %v", a, m)
	}
}

func TestChtimesPaths(t *testing.T) {
	fs, _ := formatChtimes(t)
	defer fs.Close()
	now := time.Now()
	for _, p := range []string{"/NOPE.TXT", "/NOPE/X.TXT"} {
		if err := fs.Chtimes(p, now, now); !errors.Is(err, iofs.ErrNotExist) {
			t.Errorf("%s: %v, want ErrNotExist", p, err)
		}
	}
	if err := fs.Chtimes("/NOPE.TXT", time.Time{}, time.Time{}); !errors.Is(err, iofs.ErrNotExist) {
		t.Errorf("zero times on a missing path: %v", err)
	}
	if err := fs.Chtimes("/", now, now); err == nil {
		t.Error("the root has no entry: want an error")
	}
	if err := fs.Chtimes("A.TXT", now, now); err == nil {
		t.Error("relative path accepted")
	}
}

func TestChtimesReadOnly(t *testing.T) {
	_, img := formatChtimes(t)
	raw, err := os.ReadFile(img)
	if err != nil {
		t.Fatal(err)
	}
	ro, err := OpenReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	now := time.Now()
	if err := ro.(chtimesFS).Chtimes("/A.TXT", now, now); !errors.Is(err, ErrReadOnlyReader) {
		t.Errorf("Chtimes on a read-only reader = %v, want ErrReadOnlyReader", err)
	}
}

// TestChtimesFaultInjection: a failed read or write of the directory cluster is
// returned, and the entry keeps its old times and a valid checksum.
func TestChtimesFaultInjection(t *testing.T) {
	setLocal(t, time.UTC)
	fsi, _ := formatChtimes(t)
	fs := fsi.(*exfatFS)
	defer fs.Close()
	old := time.Date(2010, 1, 2, 3, 4, 6, 0, time.UTC)
	if err := fs.Chtimes("/A.TXT", old, old); err != nil {
		t.Fatal(err)
	}
	nw := time.Date(2030, 7, 8, 9, 10, 12, 0, time.UTC)
	rootLo := fs.info.ClusterHeapOffsetBytes(fs.partOffset) + int64(fs.info.RootDirectoryCluster-2)*int64(fs.info.ClusterSize())
	rootHi := rootLo + int64(fs.info.ClusterSize())

	for _, kind := range []string{"read", "write"} {
		w := wrapFault(fs)
		if kind == "read" {
			w.failReadLo, w.failReadHi = rootLo, rootHi
		} else {
			w.failWriteLo, w.failWriteHi = rootLo, rootHi
		}
		err := fs.Chtimes("/A.TXT", nw, nw)
		fs.f = w.inner // disarm
		if !errors.Is(err, errInjected) {
			t.Fatalf("%s fault: err = %v", kind, err)
		}
		if a, m := timesOf(t, fs, "/A.TXT"); !m.Equal(old) || !a.Equal(old) {
			t.Errorf("%s fault: the times changed to %v %v", kind, a, m)
		}
		buf, err := fs.readDirBuf(fs.info.RootDirectoryCluster)
		if err != nil {
			t.Fatal(err)
		}
		off, n := exfatFindEntry(buf, "A.TXT")
		e := buf[off : off+(n+1)*dirEntrySize]
		if binary.LittleEndian.Uint16(e[2:]) != exfatEntrySetChecksum(e) {
			t.Errorf("%s fault: the entry set no longer checksums", kind)
		}
	}
	if err := fs.Chtimes("/A.TXT", nw, nw); err != nil {
		t.Fatalf("after the faults: %v", err)
	}
	if _, m := timesOf(t, fs, "/A.TXT"); !m.Equal(nw) {
		t.Errorf("mtime %v", m)
	}
}

// TestChtimesMacOS has Apple's exFAT implementation read the result: fsck_exfat
// accepts the volume (entry-set checksums included) and the mounted file's
// stat shows the times that were set. It needs macOS and mounts a disk image, so
// it runs only with EXFAT_MACOS_VERIFY=1.
func TestChtimesMacOS(t *testing.T) {
	if os.Getenv("EXFAT_MACOS_VERIFY") != "1" {
		t.Skip("set EXFAT_MACOS_VERIFY=1 (macOS) to mount an image and stat it")
	}
	setLocal(t, time.FixedZone("CET", 3600))
	img := filepath.Join(t.TempDir(), "mac.img")
	fs, err := Format(img, 16<<20, FormatConfig{Label: "CHT"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]time.Time{
		"/A.TXT": {time.Date(2024, 5, 18, 8, 0, 10, 0, time.Local), time.Date(2024, 5, 17, 13, 45, 30, 0, time.Local)},
		"/B.TXT": {time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)},
		"/C.TXT": {time.Date(2107, 12, 31, 22, 59, 58, 0, time.UTC), time.Date(2107, 12, 31, 22, 59, 58, 0, time.UTC)}, // 23:59:58 local
	}
	for p, v := range want {
		if err := fs.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := fs.(chtimesFS).Chtimes(p, v[0], v[1]); err != nil {
			t.Fatal(err)
		}
	}
	fs.Close()
	run := func(name string, args ...string) string {
		t.Helper()
		cmd := exec.Command(name, args...)
		var stderr strings.Builder
		cmd.Stderr = &stderr // hdiutil warns on stderr; stdout carries the device
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s%s", name, args, err, out, stderr.String())
		}
		return string(out)
	}
	disk := strings.Fields(run("hdiutil", "attach", "-imagekey", "diskimage-class=CRawDiskImage", "-nomount", img))[0]
	detach := func() { _ = exec.Command("hdiutil", "detach", disk).Run() }
	defer detach()
	t.Logf("fsck_exfat -n:\n%s", run("fsck_exfat", "-n", disk))
	run("diskutil", "mount", disk)
	mnt := "/Volumes/CHT"
	defer func() { _ = exec.Command("diskutil", "unmount", mnt).Run() }()
	for p, v := range want {
		// stat -f %a/%m: seconds since the epoch, independent of the zone.
		out := strings.Fields(run("stat", "-f", "%a %m", filepath.Join(mnt, p)))
		a, _ := strconv.ParseInt(out[0], 10, 64)
		m, _ := strconv.ParseInt(out[1], 10, 64)
		if a != v[0].Unix() || m != v[1].Unix() {
			t.Errorf("%s: macOS stat atime %d mtime %d, want %d %d", p, a, m, v[0].Unix(), v[1].Unix())
		}
	}
}

// encodeDecode is the pure round trip the fuzzer drives.
func encodeDecode(atime, mtime time.Time) (filesystem.FileTimes, bool) {
	a, ok1 := encodeExfatTime(atime)
	m, ok2 := encodeExfatTime(mtime)
	if !ok1 || !ok2 {
		return filesystem.FileTimes{}, false
	}
	e := make([]byte, 17)
	binary.LittleEndian.PutUint32(e[4:], m.ts)
	binary.LittleEndian.PutUint32(e[8:], a.ts)
	e[13], e[15], e[16] = m.inc, m.off, a.off
	return decodeExfatTimes(e), true
}

func FuzzChtimesEncode(f *testing.F) {
	f.Add(int64(315532800), int64(0), int64(315532800), int64(0), int32(0))
	f.Add(int64(4354819199), int64(999_999_999), int64(4354819198), int64(999_999_999), int32(3600))
	f.Add(int64(-1), int64(5), int64(4354819200), int64(0), int32(-5*3600))
	f.Add(int64(1700000000), int64(350_000_000), int64(1700000001), int64(10_000_000), int32(5*3600+1800))
	f.Fuzz(func(t *testing.T, as, an, ms, mn int64, zoneSec int32) {
		if as < -1<<40 || as > 1<<40 || ms < -1<<40 || ms > 1<<40 {
			t.Skip()
		}
		setLocal(t, time.FixedZone("Z", int(zoneSec%(20*3600))))
		at, mt := time.Unix(as, an%1_000_000_000), time.Unix(ms, mn%1_000_000_000)
		ft, ok := encodeDecode(at, mt)
		if !ok {
			return
		}
		a, ok1 := ft.Atime.Get()
		m, ok2 := ft.Mtime.Get()
		if !ok1 || !ok2 {
			t.Fatalf("%v %v encoded to an unrecorded time", at, mt)
		}
		if !a.Equal(at.Truncate(2*time.Second)) || !m.Equal(mt.Truncate(10*time.Millisecond)) {
			t.Fatalf("%v %v (zone %d) round-tripped to %v %v", at, mt, zoneSec, a, m)
		}
	})
}
