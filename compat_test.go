package filesystem_exfat

import (
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

// ── Cross-compatibility tests against canonical exFAT tools ───────────────
//
// These tests verify bidirectional interop between this driver and the
// reference Linux/macOS exFAT tooling:
//
//   * TestReadMkfsImage  — reads a committed fixture produced by an
//                          external formatter (newfs_exfat on macOS or
//                          mkfs.exfat from exfatprogs on Linux). The
//                          fixture lives in testdata/mkfs/.
//   * TestWriteThenFsck  — formats a fresh image with our own writer,
//                          drops a file into it, and runs the canonical
//                          fsck binary against the result, asserting
//                          a clean (no-errors) verdict.
//   * TestRegenerateMkfs — opt-in (set EXFAT_REGENERATE_FIXTURE=1) rebuild
//                          of testdata/mkfs/image.exfat.gz from scratch
//                          on the local box. Useful for refreshing the
//                          fixture deterministically; not part of the
//                          default test run.
//
// Every test t.Skip()s with a clear message if the tooling it needs is
// missing, so `go test ./...` still passes on bare developer machines.

// ── Tool lookup helpers ──────────────────────────────────────────────────

// canonicalMkfsTool returns the absolute path of the canonical exFAT
// formatter found on PATH, plus a human-readable name. Empty path means
// no tool is available. Linux ships mkfs.exfat (exfatprogs); macOS
// ships newfs_exfat in /sbin.
func canonicalMkfsTool() (path, name string) {
	for _, candidate := range []string{"mkfs.exfat", "newfs_exfat"} {
		if p, err := exec.LookPath(candidate); err == nil {
			return p, candidate
		}
	}
	// Fall back to known macOS install path even when /sbin isn't in PATH.
	if runtime.GOOS == "darwin" {
		if _, err := os.Stat("/sbin/newfs_exfat"); err == nil {
			return "/sbin/newfs_exfat", "newfs_exfat"
		}
	}
	return "", ""
}

// canonicalFsckTool returns the canonical exFAT checker plus its name.
// Linux ships fsck.exfat (exfatprogs); macOS ships fsck_exfat in /sbin.
func canonicalFsckTool() (path, name string) {
	for _, candidate := range []string{"fsck.exfat", "fsck_exfat"} {
		if p, err := exec.LookPath(candidate); err == nil {
			return p, candidate
		}
	}
	if runtime.GOOS == "darwin" {
		if _, err := os.Stat("/sbin/fsck_exfat"); err == nil {
			return "/sbin/fsck_exfat", "fsck_exfat"
		}
	}
	return "", ""
}

// ── Read-side: TestReadMkfsImage ─────────────────────────────────────────

// expectedFile mirrors one of the lines in testdata/mkfs/EXPECTED.txt:
// md5 (hex), size (bytes), virtual path inside the image.
type expectedFile struct {
	md5  string
	size int64
	path string
}

// expectedMkfsFiles are the files known to live inside the committed
// fixture. Keep this in sync with testdata/mkfs/EXPECTED.txt.
var expectedMkfsFiles = []expectedFile{
	{md5: "b1946ac92492d2347c6235b4d2611184", size: 6, path: "/hello.txt"},
	{md5: "d47b127bc2de2d687ddc82dac354c415", size: 1024, path: "/sub/blob.bin"},
}

// extractGz decompresses src (a path to a *.gz file) into a freshly created
// file at dst. The destination is created with 0o600 so our O_RDWR open
// in exfat.Open succeeds.
func extractGz(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()
	gz, err := gzip.NewReader(in)
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}
	defer gz.Close()
	out, err := os.OpenFile(dst, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	if _, err := io.Copy(out, gz); err != nil {
		out.Close()
		return fmt.Errorf("decompress: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", dst, err)
	}
	return nil
}

func md5Hex(data []byte) string {
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}

// TestReadMkfsImage opens the committed canonical fixture (produced by
// newfs_exfat / mkfs.exfat) and verifies the driver decodes every known
// file exactly as expected. No external tooling is needed at test time
// because the fixture is committed.
func TestReadMkfsImage(t *testing.T) {
	src := filepath.Join("testdata", "mkfs", "image.exfat.gz")
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("fixture %s missing: %v", src, err)
	}
	dst := filepath.Join(t.TempDir(), "image.exfat")
	if err := extractGz(src, dst); err != nil {
		t.Fatalf("extractGz: %v", err)
	}

	fsIfc, err := Open(dst, -1)
	if err != nil {
		t.Fatalf("Open canonical fixture: %v", err)
	}
	t.Cleanup(func() { _ = fsIfc.Close() })

	// LabelReader is optional — verify the committed fixture's label
	// when the driver exposes one.
	if lr, ok := fsIfc.(filesystem.LabelReader); ok {
		if got := lr.Label(); got != "" && !strings.EqualFold(got, "EXFATTEST") {
			t.Errorf("Label() = %q, want %q", got, "EXFATTEST")
		}
	}

	// Root directory listing must contain hello.txt and sub.
	rootEntries, err := fsIfc.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir(/): %v", err)
	}
	have := map[string]bool{}
	for _, e := range rootEntries {
		have[e.Name()] = true
	}
	for _, want := range []string{"hello.txt", "sub"} {
		if !have[want] {
			t.Errorf("ListDir(/) missing %q (got %v)", want, rootEntries)
		}
	}

	// /sub directory listing must contain blob.bin.
	subEntries, err := fsIfc.ListDir("/sub")
	if err != nil {
		t.Fatalf("ListDir(/sub): %v", err)
	}
	subHave := map[string]bool{}
	for _, e := range subEntries {
		subHave[e.Name()] = true
	}
	if !subHave["blob.bin"] {
		t.Errorf("ListDir(/sub) missing blob.bin (got %v)", subEntries)
	}

	// Verify each expected file's size + md5 via ReadFile.
	for _, ef := range expectedMkfsFiles {
		st, err := fsIfc.Stat(ef.path)
		if err != nil {
			t.Errorf("Stat(%q): %v", ef.path, err)
			continue
		}
		if int64(st.Size()) != ef.size {
			t.Errorf("Stat(%q).Size() = %d, want %d", ef.path, st.Size(), ef.size)
		}
		body, err := fsIfc.ReadFile(ef.path)
		if err != nil {
			t.Errorf("ReadFile(%q): %v", ef.path, err)
			continue
		}
		if int64(len(body)) != ef.size {
			t.Errorf("ReadFile(%q) len = %d, want %d", ef.path, len(body), ef.size)
		}
		if got := md5Hex(body); got != ef.md5 {
			t.Errorf("ReadFile(%q) md5 = %s, want %s", ef.path, got, ef.md5)
		}
	}
}

// ── Write-side: TestWriteThenFsckExfat ───────────────────────────────────

// runFsck invokes the canonical fsck binary in read-only mode against the
// image (or raw device) at target. Returns the combined stdout/stderr
// output and whatever error exec reports. The caller decides how to
// interpret the verdict; this helper just shells out.
//
// On Linux fsck.exfat happily operates on a plain file. macOS fsck_exfat
// requires a character-special device, so the caller is responsible for
// attaching the image via hdiutil first and passing /dev/rdiskN.
func runFsck(t *testing.T, fsckBin, target string) (string, error) {
	t.Helper()
	cmd := exec.Command(fsckBin, "-n", target)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// attachAsRawDevice attaches imgPath as a raw character device on macOS
// via `hdiutil attach -nomount`. Returns the raw device path (/dev/rdiskN)
// and a cleanup function. Errors out the test on failure.
func attachAsRawDevice(t *testing.T, imgPath string) (string, func()) {
	t.Helper()
	out, err := exec.Command("hdiutil",
		"attach", "-nomount",
		"-imagekey", "diskimage-class=CRawDiskImage",
		imgPath,
	).Output()
	if err != nil {
		t.Skipf("hdiutil attach failed (need elevated permissions to test fsck_exfat on raw image): %v", err)
	}
	dev := strings.TrimSpace(string(out))
	if !strings.HasPrefix(dev, "/dev/disk") {
		t.Skipf("hdiutil attach produced unexpected output %q", dev)
	}
	cleanup := func() {
		_ = exec.Command("hdiutil", "detach", "-force", dev).Run()
	}
	raw := strings.Replace(dev, "/dev/disk", "/dev/rdisk", 1)
	return raw, cleanup
}

// fsckLooksClean inspects a fsck run for the expected "no errors" /
// "clean" verdict. Different fsck implementations word their verdicts
// differently across versions:
//
//   - exfatprogs (Linux):       "No error found"
//   - exfatprogs 1.2.9 (Linux): "<path>: clean. directories N, files M"
//   - Apple fsck_exfat:         "The volume … appears to be OK" (or
//     "clean").
//
// Rather than chase every exact phrase, we treat a run as clean when the
// checker exited 0 and its output mentions "clean" (or one of the legacy
// "no errors"/"OK" verdicts) without any error/corruption marker. The
// errors-found / "would fix" rc paths are handled by the caller, which
// surfaces the output before failing.
//
// runErr is the error returned by exec (non-nil ⇒ non-zero exit). A
// non-zero exit is never treated as clean. We still refuse to call output
// clean if it carries a corruption marker, so a real driver bug cannot be
// papered over.
func fsckLooksClean(out string, runErr error) bool {
	if runErr != nil {
		// Non-zero exit: the checker is signalling a problem (or that it
		// would fix something). Not clean.
		return false
	}
	low := strings.ToLower(out)
	// Hard "this is broken" markers veto a clean verdict regardless of exit
	// code, so we never ignore genuine corruption.
	for _, bad := range []string{
		"corrupt",
		"error",
		"errors found",
		"would fix",
		"need to repair",
	} {
		if strings.Contains(low, bad) {
			// "no error found" / "no errors" contains "error" but is a
			// clean verdict, so re-check those explicitly below.
			if strings.Contains(low, "no error") {
				continue
			}
			return false
		}
	}
	for _, marker := range []string{
		"no error found",
		"no errors",
		"appears to be ok",
		"clean",
	} {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}

// TestWriteThenFsckExfat writes a fresh exFAT image with the driver's own
// Format+WriteFile and then asks the canonical fsck to validate it. The
// test is skipped when fsck.exfat / fsck_exfat is not available.
func TestWriteThenFsckExfat(t *testing.T) {
	fsckBin, fsckName := canonicalFsckTool()
	if fsckBin == "" {
		t.Skip("canonical exFAT checker not on PATH; install exfatprogs (Linux: fsck.exfat) or rely on /sbin/fsck_exfat (macOS Big Sur+)")
	}

	imgPath := filepath.Join(t.TempDir(), "written.img")
	const sizeBytes = 4 * 1024 * 1024 // 4 MiB
	fsIfc, err := Format(imgPath, sizeBytes, FormatConfig{Label: "WRITERTEST"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	const payload = "round-trip via fsck\n"
	if err := fsIfc.WriteFile("/hello.txt", []byte(payload), 0o644); err != nil {
		fsIfc.Close()
		t.Fatalf("WriteFile: %v", err)
	}
	if err := fsIfc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Decide the target fsck expects.
	target := imgPath
	if fsckName == "fsck_exfat" {
		// macOS fsck_exfat needs a character-special device, not a file.
		raw, cleanup := attachAsRawDevice(t, imgPath)
		defer cleanup()
		target = raw
	}

	out, err := runFsck(t, fsckBin, target)
	t.Logf("%s -n %s output:\n%s", fsckName, target, out)
	if !fsckLooksClean(out, err) {
		t.Fatalf("%s did not report a clean filesystem (err=%v):\n%s", fsckName, err, out)
	}
}

// ── Regenerate the canonical fixture (opt-in) ────────────────────────────

// regenerateMkfsImage rebuilds testdata/mkfs/image.exfat.gz from scratch
// using the canonical formatter found on the local box. It mounts the
// fresh image via hdiutil (macOS) or loop device (Linux), drops the
// known files, then gzips the result back into testdata/mkfs/.
//
// Returns nil on success, an error otherwise. The caller is responsible
// for deciding whether to invoke this (it requires the canonical tools
// and, on Linux, root privileges for loop mounts — so it is not run by
// default).
func regenerateMkfsImage(t *testing.T) error {
	t.Helper()
	mkfsBin, mkfsName := canonicalMkfsTool()
	if mkfsBin == "" {
		return errors.New("no canonical exFAT formatter on PATH")
	}
	if runtime.GOOS != "darwin" {
		// Linux path is intentionally not wired up here: it needs
		// `sudo mount -o loop` which can't run unattended in `go test`.
		return fmt.Errorf("regenerate path only implemented for darwin; got %s", runtime.GOOS)
	}

	tmp := t.TempDir()
	rawImg := filepath.Join(tmp, "fixture.img")
	if err := os.WriteFile(rawImg, make([]byte, 512*1024), 0o600); err != nil {
		return fmt.Errorf("create blank image: %w", err)
	}

	// Attach as a raw character disk image so newfs_exfat can format it.
	attachOut, err := exec.Command("hdiutil",
		"attach", "-nomount",
		"-imagekey", "diskimage-class=CRawDiskImage",
		rawImg,
	).Output()
	if err != nil {
		return fmt.Errorf("hdiutil attach: %w", err)
	}
	dev := strings.TrimSpace(string(attachOut))
	defer func() { _ = exec.Command("hdiutil", "detach", "-force", dev).Run() }()

	if out, err := exec.Command(mkfsBin, "-v", "EXFATTEST", dev).CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w\n%s", mkfsName, err, out)
	}

	// Mount the freshly-formatted volume and populate the known files.
	mnt := filepath.Join(tmp, "mnt")
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		return fmt.Errorf("mkdir mount point: %w", err)
	}
	if out, err := exec.Command("mount", "-t", "exfat", dev, mnt).CombinedOutput(); err != nil {
		return fmt.Errorf("mount: %w\n%s", err, out)
	}
	defer func() { _ = exec.Command("umount", mnt).Run() }()

	if err := os.WriteFile(filepath.Join(mnt, "hello.txt"), []byte("hello\n"), 0o644); err != nil {
		return fmt.Errorf("write hello.txt: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(mnt, "sub"), 0o755); err != nil {
		return fmt.Errorf("mkdir /sub: %w", err)
	}
	blob := bytes.Repeat([]byte{'A'}, 1024)
	if err := os.WriteFile(filepath.Join(mnt, "sub", "blob.bin"), blob, 0o644); err != nil {
		return fmt.Errorf("write blob.bin: %w", err)
	}

	// Strip macOS-side noise (AppleDouble files, .fseventsd) before
	// committing — they're not part of the intended fixture surface.
	for _, victim := range []string{
		filepath.Join(mnt, "._hello.txt"),
		filepath.Join(mnt, "._sub"),
		filepath.Join(mnt, "sub", "._blob.bin"),
	} {
		_ = os.Remove(victim)
	}
	_ = os.RemoveAll(filepath.Join(mnt, ".fseventsd"))

	// Unmount before reading the raw bytes (avoid catching a stale
	// in-kernel write cache).
	if out, err := exec.Command("umount", mnt).CombinedOutput(); err != nil {
		return fmt.Errorf("umount: %w\n%s", err, out)
	}
	if out, err := exec.Command("hdiutil", "detach", "-force", dev).CombinedOutput(); err != nil {
		return fmt.Errorf("hdiutil detach: %w\n%s", err, out)
	}

	// Gzip the raw image into testdata/mkfs/.
	srcBytes, err := os.ReadFile(rawImg)
	if err != nil {
		return fmt.Errorf("read regenerated image: %w", err)
	}
	dstPath := filepath.Join("testdata", "mkfs", "image.exfat.gz")
	out, err := os.OpenFile(dstPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", dstPath, err)
	}
	gz, _ := gzip.NewWriterLevel(out, gzip.BestCompression)
	if _, err := gz.Write(srcBytes); err != nil {
		_ = gz.Close()
		_ = out.Close()
		return fmt.Errorf("gzip write: %w", err)
	}
	if err := gz.Close(); err != nil {
		_ = out.Close()
		return fmt.Errorf("gzip close: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close gz: %w", err)
	}
	t.Logf("regenerated %s (%d bytes raw → %d bytes gz)", dstPath, len(srcBytes), fileSize(dstPath))
	return nil
}

func fileSize(p string) int64 {
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// TestRegenerateMkfsImage is opt-in: set EXFAT_REGENERATE_FIXTURE=1 to
// rebuild testdata/mkfs/image.exfat.gz on the local box. Useful when the
// fixture needs refreshing; the committed image is otherwise stable
// across runs.
func TestRegenerateMkfsImage(t *testing.T) {
	if os.Getenv("EXFAT_REGENERATE_FIXTURE") != "1" {
		t.Skip("set EXFAT_REGENERATE_FIXTURE=1 to rebuild testdata/mkfs/image.exfat.gz")
	}
	if _, _ = canonicalMkfsTool(); false {
		// (kept for parity; regenerateMkfsImage rechecks)
	}
	if err := regenerateMkfsImage(t); err != nil {
		t.Fatalf("regenerate fixture: %v", err)
	}

	// Sanity check: the regenerated fixture must still satisfy the read-side
	// test expectations.
	dst := filepath.Join(t.TempDir(), "image.exfat")
	if err := extractGz(filepath.Join("testdata", "mkfs", "image.exfat.gz"), dst); err != nil {
		t.Fatalf("extractGz: %v", err)
	}
	fsIfc, err := Open(dst, -1)
	if err != nil {
		t.Fatalf("Open regenerated fixture: %v", err)
	}
	defer fsIfc.Close()
	for _, ef := range expectedMkfsFiles {
		body, err := fsIfc.ReadFile(ef.path)
		if err != nil {
			t.Errorf("ReadFile(%q): %v", ef.path, err)
			continue
		}
		if got := md5Hex(body); got != ef.md5 {
			t.Errorf("regenerated %q md5 = %s, want %s", ef.path, got, ef.md5)
		}
	}
}
