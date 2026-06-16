package filesystem_exfat

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ── Helpers ──────────────────────────────────────────────────────────────

// freshFormattedFS formats a brand-new exFAT image of size bytes at a
// temp path, returns the *exfatFS plus the path. Closes/reopens are
// the caller's responsibility.
func freshFormattedFS(t *testing.T, size int64) (*exfatFS, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "resize.img")
	fsIfc, err := Format(path, size, FormatConfig{Label: "RESIZETEST"})
	if err != nil {
		t.Fatalf("Format(%d): %v", size, err)
	}
	t.Cleanup(func() { _ = fsIfc.Close() })
	return fsIfc.(*exfatFS), path
}

// reopenAs reopens the image at path and returns the concrete *exfatFS.
func reopenAs(t *testing.T, path string) *exfatFS {
	t.Helper()
	fsIfc, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = fsIfc.Close() })
	return fsIfc.(*exfatFS)
}

// ── Resize switching ─────────────────────────────────────────────────────

func TestResize_NoOpWhenSizeMatches(t *testing.T) {
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	cur := int64(fs.info.VolumeLength) * int64(fs.info.BytesPerSector())
	if err := fs.Resize(cur); err != nil {
		t.Fatalf("Resize(curSize): %v", err)
	}
}

func TestResize_DispatchesGrow(t *testing.T) {
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	cur := int64(fs.info.VolumeLength) * int64(fs.info.BytesPerSector())
	if err := fs.Resize(cur + int64(fs.info.ClusterSize())); err != nil {
		t.Fatalf("Resize grow: %v", err)
	}
	newCur := int64(fs.info.VolumeLength) * int64(fs.info.BytesPerSector())
	if newCur != cur+int64(fs.info.ClusterSize()) {
		t.Errorf("post-grow size = %d, want %d", newCur, cur+int64(fs.info.ClusterSize()))
	}
}

func TestResize_DispatchesShrink(t *testing.T) {
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	cur := int64(fs.info.VolumeLength) * int64(fs.info.BytesPerSector())
	target := cur - int64(fs.info.ClusterSize())
	if err := fs.Resize(target); err != nil {
		t.Fatalf("Resize shrink: %v", err)
	}
	newCur := int64(fs.info.VolumeLength) * int64(fs.info.BytesPerSector())
	if newCur != target {
		t.Errorf("post-shrink size = %d, want %d", newCur, target)
	}
}

// ── Grow happy path ──────────────────────────────────────────────────────

func TestGrow_UpdatesVolumeLengthAndClusterCount(t *testing.T) {
	const startSize = 4 * 1024 * 1024
	const endSize = 6 * 1024 * 1024
	fs, path := freshFormattedFS(t, startSize)
	oldClusterCount := fs.info.ClusterCount
	if err := fs.Grow(endSize); err != nil {
		t.Fatalf("Grow: %v", err)
	}
	if fs.info.VolumeLength*uint64(fs.info.BytesPerSector()) != endSize {
		t.Errorf("VolumeLength after grow = %d sectors", fs.info.VolumeLength)
	}
	if fs.info.ClusterCount <= oldClusterCount {
		t.Errorf("ClusterCount didn't increase: %d -> %d", oldClusterCount, fs.info.ClusterCount)
	}

	// Re-open and verify it persists.
	_ = fs.Close()
	fs2 := reopenAs(t, path)
	if fs2.info.VolumeLength*uint64(fs2.info.BytesPerSector()) != endSize {
		t.Errorf("after reopen VolumeLength = %d sectors", fs2.info.VolumeLength)
	}
	if fs2.info.ClusterCount != fs.info.ClusterCount {
		t.Errorf("after reopen ClusterCount = %d, want %d", fs2.info.ClusterCount, fs.info.ClusterCount)
	}
	// File on disk must be the right size.
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Size() != endSize {
		t.Errorf("file size = %d, want %d", st.Size(), endSize)
	}
}

func TestGrow_PreservesUserData(t *testing.T) {
	const startSize = 4 * 1024 * 1024
	const endSize = 8 * 1024 * 1024
	fs, path := freshFormattedFS(t, startSize)
	payload := bytes.Repeat([]byte{0xAB}, 2048)
	if err := fs.WriteFile("/keep.bin", payload, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := fs.Grow(endSize); err != nil {
		t.Fatalf("Grow: %v", err)
	}
	_ = fs.Close()
	fs2 := reopenAs(t, path)
	got, err := fs2.ReadFile("/keep.bin")
	if err != nil {
		t.Fatalf("ReadFile after grow: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("user data corrupted by grow")
	}
}

func TestGrow_BootChecksumStaysConsistent(t *testing.T) {
	const startSize = 4 * 1024 * 1024
	const endSize = 6 * 1024 * 1024
	fs, path := freshFormattedFS(t, startSize)
	if err := fs.Grow(endSize); err != nil {
		t.Fatalf("Grow: %v", err)
	}
	_ = fs.Close()
	// Read raw boot region, compute checksum, verify it matches the
	// stored value in sector 11.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read image: %v", err)
	}
	bytesPerSector := int64(1) << raw[108]
	region := raw[:11*bytesPerSector]
	stored := binary.LittleEndian.Uint32(raw[11*bytesPerSector:])
	want := exfatBootChecksum(region)
	if stored != want {
		t.Errorf("main boot checksum mismatch after grow: stored=%08x want=%08x", stored, want)
	}
	storedBackup := binary.LittleEndian.Uint32(raw[23*bytesPerSector:])
	if storedBackup != want {
		t.Errorf("backup boot checksum mismatch after grow: stored=%08x want=%08x", storedBackup, want)
	}
}

// ── Shrink happy path ────────────────────────────────────────────────────

func TestShrink_UpdatesVolumeLengthAndClusterCount(t *testing.T) {
	const startSize = 8 * 1024 * 1024
	const endSize = 4 * 1024 * 1024
	fs, path := freshFormattedFS(t, startSize)
	if err := fs.Shrink(endSize); err != nil {
		t.Fatalf("Shrink: %v", err)
	}
	if fs.info.VolumeLength*uint64(fs.info.BytesPerSector()) != endSize {
		t.Errorf("VolumeLength after shrink = %d sectors", fs.info.VolumeLength)
	}
	_ = fs.Close()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Size() != endSize {
		t.Errorf("file size after shrink = %d, want %d", st.Size(), endSize)
	}
	fs2 := reopenAs(t, path)
	if fs2.info.VolumeLength*uint64(fs2.info.BytesPerSector()) != endSize {
		t.Errorf("reopen VolumeLength = %d sectors", fs2.info.VolumeLength)
	}
}

// ── Shrink refusal ───────────────────────────────────────────────────────

func TestShrink_RefusesWhenClustersAreInTheWay(t *testing.T) {
	// 8 MiB image gives us comfortably more than 1024 data clusters.
	const startSize = 8 * 1024 * 1024
	fs, _ := freshFormattedFS(t, startSize)
	// Force-allocate a cluster near the end of the volume in the FAT.
	// We bypass writeData on purpose: writeData would pick the *first*
	// free cluster and shrink would just be allowed.
	highCluster := fs.info.ClusterCount + 1 // last addressable cluster
	if err := fs.setFATEntry(highCluster, 0xFFFFFFFF); err != nil {
		t.Fatalf("setFATEntry: %v", err)
	}
	if err := fs.setBitmapBit(highCluster, true); err != nil {
		t.Fatalf("setBitmapBit: %v", err)
	}
	// Try to shrink to half the size — the last cluster is now beyond
	// the new heap and shrink must refuse.
	if err := fs.Shrink(4 * 1024 * 1024); err == nil {
		t.Fatal("Shrink unexpectedly succeeded with allocated cluster in the way")
	} else if !strings.Contains(err.Error(), "allocated") {
		t.Errorf("error %q does not mention an allocated cluster", err)
	}
}

func TestShrink_SucceedsViaFATFallback(t *testing.T) {
	// FAT-fallback path with no allocated clusters in the way: the
	// shrink should succeed. We zero bitmapCluster on the live fs to
	// force the FAT scan to be used (mirrors an older minimal image
	// that lacked the bitmap entry).
	fs, _ := freshFormattedFS(t, 8*1024*1024)
	fs.bitmapCluster = 0
	fs.bitmapLength = 0
	if err := fs.Shrink(6 * 1024 * 1024); err != nil {
		t.Fatalf("FAT-fallback shrink: %v", err)
	}
}

func TestShrink_RefusesViaFATFallback(t *testing.T) {
	// Older minimal images may lack a bitmap entry; emulate that case
	// by zeroing the bitmap reference on the live fs and dirtying a
	// late FAT entry directly.
	const startSize = 8 * 1024 * 1024
	fs, _ := freshFormattedFS(t, startSize)
	fs.bitmapCluster = 0
	fs.bitmapLength = 0
	highCluster := fs.info.ClusterCount + 1
	if err := fs.setFATEntry(highCluster, 0xFFFFFFFF); err != nil {
		t.Fatalf("setFATEntry: %v", err)
	}
	if err := fs.Shrink(4 * 1024 * 1024); err == nil {
		t.Fatal("Shrink unexpectedly succeeded via FAT-fallback path")
	} else if !strings.Contains(err.Error(), "allocated") {
		t.Errorf("error %q does not mention an allocated cluster", err)
	}
}

// ── Validation errors ───────────────────────────────────────────────────

func TestResize_RejectsNegative(t *testing.T) {
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	if err := fs.Resize(-1); err == nil {
		t.Error("Resize(-1) should fail")
	}
	if err := fs.Grow(-1); err == nil {
		t.Error("Grow(-1) should fail")
	}
	if err := fs.Shrink(-1); err == nil {
		t.Error("Shrink(-1) should fail")
	}
}

func TestResize_RejectsUnalignedSize(t *testing.T) {
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	if err := fs.Grow(5*1024*1024 + 1); err == nil {
		t.Error("Grow with unaligned size should fail")
	}
}

func TestGrow_RejectsShrinkRange(t *testing.T) {
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	if err := fs.Grow(2 * 1024 * 1024); err == nil {
		t.Error("Grow with smaller size should fail")
	}
	cur := int64(fs.info.VolumeLength) * int64(fs.info.BytesPerSector())
	if err := fs.Grow(cur); err == nil {
		t.Error("Grow with equal size should fail")
	}
}

func TestShrink_RejectsGrowRange(t *testing.T) {
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	if err := fs.Shrink(8 * 1024 * 1024); err == nil {
		t.Error("Shrink with larger size should fail")
	}
	cur := int64(fs.info.VolumeLength) * int64(fs.info.BytesPerSector())
	if err := fs.Shrink(cur); err == nil {
		t.Error("Shrink with equal size should fail")
	}
}

func TestResize_RejectsHeadlessSize(t *testing.T) {
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	// A size smaller than the cluster heap offset can never be valid.
	heapOffsetBytes := int64(fs.info.ClusterHeapOffset) * int64(fs.info.BytesPerSector())
	if err := fs.Shrink(heapOffsetBytes); err == nil {
		t.Error("Shrink down to cluster-heap offset should fail")
	}
}

func TestResize_RejectsTooFewClusters(t *testing.T) {
	// We need a target size that leaves *some* room for the cluster
	// heap (so we get past the "no room" branch) but leaves <3 data
	// clusters (so we hit the dedicated min-clusters branch).
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	clusterSize := int64(fs.info.ClusterSize())
	heapOffsetBytes := int64(fs.info.ClusterHeapOffset) * int64(fs.info.BytesPerSector())
	// Heap offset + 2 data clusters → less than the 3 required.
	target := heapOffsetBytes + 2*clusterSize
	// Round up to a cluster boundary so the unaligned-size branch
	// doesn't intercept us first.
	if rem := target % clusterSize; rem != 0 {
		target += clusterSize - rem
	}
	if err := fs.Shrink(target); err == nil {
		t.Error("Shrink leaving fewer than 3 data clusters should fail")
	} else if !strings.Contains(err.Error(), "data clusters") {
		t.Errorf("error %q does not mention data clusters", err)
	}
}

func TestResize_RejectsFATOverflow(t *testing.T) {
	// Format provisions a FAT sized for fmtFATGrowthHeadroom × sizeBytes
	// of headroom. We grow past that ceiling and expect the explicit
	// FAT-capacity error to fire rather than silent corruption.
	const startSize = int64(4 * 1024 * 1024)
	fs, _ := freshFormattedFS(t, startSize)
	// Compute the FAT's actual capacity from the live boot sector and
	// request a size that pushes us strictly past it.
	fatCapacityEntries := uint64(fs.info.FATLength) *
		uint64(fs.info.BytesPerSector()) / 4
	// Bytes needed to hold a cluster heap with (fatCapacityEntries-1)
	// data clusters — i.e. one cluster beyond what the FAT can address.
	overflowClusters := fatCapacityEntries - 1 // -2 for the reserved entries +1 over
	overflowSize := (int64(fs.info.ClusterHeapOffset) +
		int64(overflowClusters)*int64(fs.info.SectorsPerCluster())) *
		int64(fs.info.BytesPerSector())
	if overflowSize <= startSize {
		t.Skipf("FAT capacity %d ≤ current size; can't synthesise overflow", fatCapacityEntries)
	}
	if err := fs.Grow(overflowSize); err == nil {
		t.Error("Grow past FAT capacity should fail")
	} else if !strings.Contains(err.Error(), "FAT capacity") {
		t.Errorf("error %q does not mention FAT capacity", err)
	}
}

// ── Repeated cycle ───────────────────────────────────────────────────────

func TestGrowShrinkCycle(t *testing.T) {
	const startSize = 4 * 1024 * 1024
	fs, path := freshFormattedFS(t, startSize)
	payload := []byte("survives grow-shrink cycles")
	if err := fs.WriteFile("/m.txt", payload, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	const midSize = 6 * 1024 * 1024
	if err := fs.Grow(midSize); err != nil {
		t.Fatalf("Grow: %v", err)
	}
	if err := fs.Shrink(startSize); err != nil {
		t.Fatalf("Shrink: %v", err)
	}
	_ = fs.Close()
	fs2 := reopenAs(t, path)
	got, err := fs2.ReadFile("/m.txt")
	if err != nil {
		t.Fatalf("ReadFile after cycle: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("data corrupted across grow-shrink cycle")
	}
	// Mutate again post-cycle and read back to ensure write paths still work.
	if err := fs2.WriteFile("/n.txt", []byte("post-cycle"), 0o644); err != nil {
		t.Fatalf("WriteFile after cycle: %v", err)
	}
	if got, _ := fs2.ReadFile("/n.txt"); string(got) != "post-cycle" {
		t.Errorf("post-cycle write/read failed: %q", got)
	}
}

// ── Backing-file Truncate failure ────────────────────────────────────────

// resizeFailingRW wraps a *os.File and lets the test force Truncate to
// fail without disturbing reads/writes.
type resizeFailingRW struct {
	inner       *os.File
	failOnGrow  bool
	failOnShrk  bool
	currentSize int64
}

func (r *resizeFailingRW) ReadAt(p []byte, off int64) (int, error)  { return r.inner.ReadAt(p, off) }
func (r *resizeFailingRW) WriteAt(p []byte, off int64) (int, error) { return r.inner.WriteAt(p, off) }
func (r *resizeFailingRW) Close() error                             { return r.inner.Close() }
func (r *resizeFailingRW) Truncate(n int64) error {
	if (r.failOnGrow && n > r.currentSize) || (r.failOnShrk && n < r.currentSize) {
		return errors.New("injected truncate failure")
	}
	r.currentSize = n
	return r.inner.Truncate(n)
}

func TestGrow_TruncateFails(t *testing.T) {
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	// Swap the underlying file for a failing wrapper.
	origFile := fs.f.(*os.File)
	fi, _ := origFile.Stat()
	fs.f = &resizeFailingRW{inner: origFile, failOnGrow: true, currentSize: fi.Size()}
	if err := fs.Grow(8 * 1024 * 1024); err == nil {
		t.Error("Grow with failing Truncate should fail")
	} else if !strings.Contains(err.Error(), "truncate") {
		t.Errorf("error %q does not mention truncate", err)
	}
}

func TestShrink_TruncateFails(t *testing.T) {
	fs, _ := freshFormattedFS(t, 8*1024*1024)
	origFile := fs.f.(*os.File)
	fi, _ := origFile.Stat()
	fs.f = &resizeFailingRW{inner: origFile, failOnShrk: true, currentSize: fi.Size()}
	if err := fs.Shrink(4 * 1024 * 1024); err == nil {
		t.Error("Shrink with failing Truncate should fail")
	} else if !strings.Contains(err.Error(), "truncate") {
		t.Errorf("error %q does not mention truncate", err)
	}
}

// resizeNonFile is a diskRW that explicitly does not implement Truncate.
type resizeNonFile struct{ inner *os.File }

func (r *resizeNonFile) ReadAt(p []byte, off int64) (int, error)  { return r.inner.ReadAt(p, off) }
func (r *resizeNonFile) WriteAt(p []byte, off int64) (int, error) { return r.inner.WriteAt(p, off) }
func (r *resizeNonFile) Close() error                             { return r.inner.Close() }

func TestResize_BackingFileWithoutTruncate(t *testing.T) {
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	origFile := fs.f.(*os.File)
	fs.f = &resizeNonFile{inner: origFile}
	if err := fs.Grow(6 * 1024 * 1024); err == nil {
		t.Error("Grow on non-truncatable backing should fail")
	} else if !strings.Contains(err.Error(), "Truncate") {
		t.Errorf("error %q does not mention Truncate", err)
	}
	if err := fs.Shrink(2 * 1024 * 1024); err == nil {
		t.Error("Shrink on non-truncatable backing should fail")
	}
}

// ── Bitmap-capacity refusal ──────────────────────────────────────────────

func TestBitmapChainCapacity_Tracks(t *testing.T) {
	// Sanity-check: a freshly-formatted volume has a single-cluster
	// bitmap chain whose capacity == clusterSize.
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	cap, err := fs.bitmapChainCapacity()
	if err != nil {
		t.Fatalf("bitmapChainCapacity: %v", err)
	}
	if cap != uint64(fs.info.ClusterSize()) {
		t.Errorf("capacity = %d, want %d", cap, fs.info.ClusterSize())
	}
}

func TestBitmapChainCapacity_NoBitmap(t *testing.T) {
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	fs.bitmapCluster = 0
	cap, err := fs.bitmapChainCapacity()
	if err != nil {
		t.Fatalf("bitmapChainCapacity: %v", err)
	}
	if cap != 0 {
		t.Errorf("capacity for no bitmap = %d, want 0", cap)
	}
}

// ── Cross-compat: resize + fsck ──────────────────────────────────────────

// TestResizeThenFsckExfat formats a fresh image, writes a file, grows,
// shrinks and asks the canonical fsck to validate the result. Skips
// with the same tooling-install hint as TestWriteThenFsckExfat.
func TestResizeThenFsckExfat(t *testing.T) {
	fsckBin, fsckName := canonicalFsckTool()
	if fsckBin == "" {
		t.Skip("canonical exFAT checker not on PATH; install exfatprogs (Linux: fsck.exfat) or rely on /sbin/fsck_exfat (macOS Big Sur+)")
	}

	imgPath := filepath.Join(t.TempDir(), "resized.img")
	const startSize = 4 * 1024 * 1024
	const growSize = 6 * 1024 * 1024
	const finalSize = 5 * 1024 * 1024
	fsIfc, err := Format(imgPath, startSize, FormatConfig{Label: "RESIZED"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	const payload = "fsck the resized image\n"
	if err := fsIfc.WriteFile("/hello.txt", []byte(payload), 0o644); err != nil {
		fsIfc.Close()
		t.Fatalf("WriteFile: %v", err)
	}

	resizer, ok := fsIfc.(interface {
		Grow(int64) error
		Shrink(int64) error
	})
	if !ok {
		fsIfc.Close()
		t.Fatalf("driver does not expose Grow/Shrink")
	}
	if err := resizer.Grow(growSize); err != nil {
		fsIfc.Close()
		t.Fatalf("Grow: %v", err)
	}
	if err := resizer.Shrink(finalSize); err != nil {
		fsIfc.Close()
		t.Fatalf("Shrink: %v", err)
	}
	if err := fsIfc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	target := imgPath
	if fsckName == "fsck_exfat" {
		raw, cleanup := attachAsRawDevice(t, imgPath)
		defer cleanup()
		target = raw
	}

	out, err := runFsck(t, fsckBin, target)
	t.Logf("%s -n %s output (after grow+shrink):\n%s", fsckName, target, out)
	if !fsckLooksClean(out, err) {
		t.Fatalf("%s did not report a clean filesystem after resize (err=%v):\n%s", fsckName, err, out)
	}
}

// ── Stress: many grow/shrink iterations ──────────────────────────────────

func TestGrowShrink_Stress(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test skipped under -short")
	}
	const baseSize = 4 * 1024 * 1024
	fs, path := freshFormattedFS(t, baseSize)
	// Drop a file and ensure it survives every iteration.
	want := bytes.Repeat([]byte("X"), 4096)
	if err := fs.WriteFile("/stress.bin", want, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	clusterSize := int64(fs.info.ClusterSize())
	cur := int64(baseSize)
	const iterations = 8
	for i := 0; i < iterations; i++ {
		// Grow by 1 cluster; then shrink back.
		target := cur + clusterSize
		if err := fs.Grow(target); err != nil {
			t.Fatalf("iteration %d Grow(%d): %v", i, target, err)
		}
		got, err := fs.ReadFile("/stress.bin")
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("iteration %d post-grow data check failed: err=%v", i, err)
		}
		if err := fs.Shrink(cur); err != nil {
			t.Fatalf("iteration %d Shrink(%d): %v", i, cur, err)
		}
		got, err = fs.ReadFile("/stress.bin")
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("iteration %d post-shrink data check failed: err=%v", i, err)
		}
	}
	_ = fs.Close()
	// One final reopen + read to make sure everything persisted.
	fs2 := reopenAs(t, path)
	got, err := fs2.ReadFile("/stress.bin")
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("final reopen read failed: err=%v", err)
	}
}

// ── Tooling presence smoke test (helps when debugging cross-compat) ──────

func TestResize_FsckBinaryDiscoverable(t *testing.T) {
	// This is informational: it's not a hard failure if the user
	// doesn't have the canonical tools installed. We just want to
	// document the discovery outcome in CI logs.
	if p, name := canonicalFsckTool(); p != "" {
		t.Logf("canonical fsck found: %s (%s)", p, name)
	} else {
		t.Logf("no canonical fsck on PATH")
	}
	if _, err := exec.LookPath("ls"); err != nil {
		t.Fatalf("environment broken: %v", err)
	}
}

// Make sure resizeFailingRW satisfies the diskRW interface at compile time.
var _ diskRW = (*resizeFailingRW)(nil)
var _ diskRW = (*resizeNonFile)(nil)

// ── ReadAt / WriteAt error injection ─────────────────────────────────────

// resizeIOFailRW is a diskRW that fails ReadAt or WriteAt at a
// specific call number. Used to cover the error branches of
// rewriteBootRegion / updateBitmapHeader / bitmap-scan helpers.
type resizeIOFailRW struct {
	inner       *os.File
	readCalls   int
	writeCalls  int
	failReadAt  int
	failWriteAt int
	currentSize int64
}

func (r *resizeIOFailRW) ReadAt(p []byte, off int64) (int, error) {
	r.readCalls++
	if r.failReadAt != 0 && r.readCalls == r.failReadAt {
		return 0, errors.New("injected ReadAt failure")
	}
	return r.inner.ReadAt(p, off)
}
func (r *resizeIOFailRW) WriteAt(p []byte, off int64) (int, error) {
	r.writeCalls++
	if r.failWriteAt != 0 && r.writeCalls == r.failWriteAt {
		return 0, errors.New("injected WriteAt failure")
	}
	return r.inner.WriteAt(p, off)
}
func (r *resizeIOFailRW) Close() error { return r.inner.Close() }
func (r *resizeIOFailRW) Truncate(n int64) error {
	r.currentSize = n
	return r.inner.Truncate(n)
}

func swapInFailingRW(fs *exfatFS, failRead, failWrite int) *resizeIOFailRW {
	orig := fs.f.(*os.File)
	fi, _ := orig.Stat()
	wrap := &resizeIOFailRW{inner: orig, failReadAt: failRead, failWriteAt: failWrite, currentSize: fi.Size()}
	fs.f = wrap
	return wrap
}

func TestGrow_BitmapChainCapacityReadFails(t *testing.T) {
	// The first ReadAt during Grow is bitmapChainCapacity's FAT read.
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	swapInFailingRW(fs, 1, 0)
	if err := fs.Grow(6 * 1024 * 1024); err == nil {
		t.Error("Grow should fail when bitmapChainCapacity read fails")
	}
}

func TestGrow_BootRegionReadFails(t *testing.T) {
	// rewriteBootRegion's main-region ReadAt is call #2 (after
	// bitmapChainCapacity's #1).
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	swapInFailingRW(fs, 2, 0)
	if err := fs.Grow(6 * 1024 * 1024); err == nil {
		t.Error("Grow should fail when boot-region read fails")
	}
}

func TestGrow_BootSectorWriteFails(t *testing.T) {
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	swapInFailingRW(fs, 0, 1)
	if err := fs.Grow(6 * 1024 * 1024); err == nil {
		t.Error("Grow should fail when writing boot sector fails")
	}
}

func TestGrow_BootChecksumWriteFails(t *testing.T) {
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	// rewriteBootRegion calls WriteAt 4 times: main boot, main checksum,
	// backup boot, backup checksum. The 2nd write is the main checksum.
	swapInFailingRW(fs, 0, 2)
	if err := fs.Grow(6 * 1024 * 1024); err == nil {
		t.Error("Grow should fail when writing boot checksum fails")
	}
}

func TestGrow_BackupBootSectorWriteFails(t *testing.T) {
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	swapInFailingRW(fs, 0, 3)
	if err := fs.Grow(6 * 1024 * 1024); err == nil {
		t.Error("Grow should fail when writing backup boot sector fails")
	}
}

func TestGrow_UpdateBitmapHeaderReadFails(t *testing.T) {
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	// rewriteBootRegion issues 1 ReadAt; updateBitmapHeader's
	// readDirBuf path is the next ReadAt. Snapshot the call count
	// after install, then fail the read that lands inside
	// updateBitmapHeader. (resize calls bitmapChainCapacity earlier;
	// that's another ReadAt. assertNoAllocBeyond is grow-only-skipped.)
	wrap := swapInFailingRW(fs, 0, 0)
	// Failed call index reached during updateBitmapHeader's readDirBuf
	// — for a default volume that's the 3rd ReadAt after the
	// bitmapChainCapacity call (1) and rewriteBootRegion's read (2).
	wrap.failReadAt = wrap.readCalls + 3
	if err := fs.Grow(6 * 1024 * 1024); err == nil {
		t.Error("Grow should fail when updateBitmapHeader read fails")
	}
}

func TestGrow_BackupChecksumWriteFails(t *testing.T) {
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	swapInFailingRW(fs, 0, 4)
	if err := fs.Grow(6 * 1024 * 1024); err == nil {
		t.Error("Grow should fail when writing backup checksum fails")
	}
}

// ── bitmapChainCapacity error path ───────────────────────────────────────

func TestBitmapChainCapacity_ReadFails(t *testing.T) {
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	swapInFailingRW(fs, 1, 0)
	if _, err := fs.bitmapChainCapacity(); err == nil {
		t.Error("bitmapChainCapacity should fail when ReadAt fails")
	}
}

// ── Shrink-time bitmap scan error path ───────────────────────────────────

func TestShrink_BitmapReadFails(t *testing.T) {
	// Fail the ReadAt that lands inside assertNoAllocBeyondViaBitmap's
	// readBitmapBytes call. For a default-format volume, the calls
	// issued before that point are:
	//   1. bitmapChainCapacity → 1 FAT read
	//   2. assertNoAllocBeyondViaBitmap → readBitmapBytes → 1 data
	//      read (no chain skip needed since startByte < clusterSize)
	// So we want to fail call #2.
	fs, _ := freshFormattedFS(t, 8*1024*1024)
	wrap := swapInFailingRW(fs, 0, 0)
	wrap.failReadAt = wrap.readCalls + 2
	if err := fs.Shrink(4 * 1024 * 1024); err == nil {
		t.Error("Shrink should fail when bitmap segment read fails")
	}
}

// ── FAT-fallback (no bitmap) ReadAt failure ──────────────────────────────

func TestShrink_FATFallback_ReadFails(t *testing.T) {
	fs, _ := freshFormattedFS(t, 8*1024*1024)
	fs.bitmapCluster = 0
	fs.bitmapLength = 0
	wrap := swapInFailingRW(fs, 0, 0)
	wrap.failReadAt = wrap.readCalls + 1
	if err := fs.Shrink(4 * 1024 * 1024); err == nil {
		t.Error("Shrink (FAT fallback) should fail when ReadAt fails")
	}
}

// ── updateBitmapHeader error paths ───────────────────────────────────────

func TestUpdateBitmapHeader_NoBitmap_IsNoop(t *testing.T) {
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	fs.bitmapCluster = 0
	if err := fs.updateBitmapHeader(123); err != nil {
		t.Errorf("updateBitmapHeader with no bitmap should be noop, got %v", err)
	}
}

// ── Root cluster validation ──────────────────────────────────────────────

func TestBitmapChainCapacity_MultiCluster(t *testing.T) {
	// Format-default bitmap is 1 cluster; extend the chain by chaining
	// cluster 2 → cluster 5 (a free cluster) then 5 → EOC. The walk
	// should report 2 clusters' worth of capacity.
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	const tailCluster = uint32(5)
	if err := fs.setFATEntry(fs.bitmapCluster, tailCluster); err != nil {
		t.Fatalf("link bitmap: %v", err)
	}
	if err := fs.setFATEntry(tailCluster, 0xFFFFFFFF); err != nil {
		t.Fatalf("set EOC: %v", err)
	}
	cap, err := fs.bitmapChainCapacity()
	if err != nil {
		t.Fatalf("bitmapChainCapacity: %v", err)
	}
	if cap != 2*uint64(fs.info.ClusterSize()) {
		t.Errorf("multi-cluster capacity = %d, want %d", cap, 2*fs.info.ClusterSize())
	}
}

func TestBitmapChainCapacity_StopsOnBadCluster(t *testing.T) {
	// FAT entry of 0 or 1 means the chain is broken — bitmapChainCapacity
	// must stop and return the count so far rather than recursing into
	// an invalid cluster index.
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	if err := fs.setFATEntry(fs.bitmapCluster, 1); err != nil {
		t.Fatalf("setFATEntry: %v", err)
	}
	cap, err := fs.bitmapChainCapacity()
	if err != nil {
		t.Fatalf("bitmapChainCapacity: %v", err)
	}
	if cap != uint64(fs.info.ClusterSize()) {
		t.Errorf("capacity = %d, want %d", cap, fs.info.ClusterSize())
	}
}

func TestReadBitmapBytes_Empty(t *testing.T) {
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	buf, err := fs.readBitmapBytes(0, 0)
	if err != nil {
		t.Fatalf("readBitmapBytes(0,0): %v", err)
	}
	if len(buf) != 0 {
		t.Errorf("expected empty buffer, got %d bytes", len(buf))
	}
}

func TestReadBitmapBytes_Single(t *testing.T) {
	// The format-default bitmap has bits 0..2 set for the bitmap,
	// upcase and root clusters. Reading byte 0 of the bitmap must
	// reveal those allocations.
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	buf, err := fs.readBitmapBytes(0, 1)
	if err != nil {
		t.Fatalf("readBitmapBytes(0,1): %v", err)
	}
	if buf[0]&0b0000_0111 != 0b0000_0111 {
		t.Errorf("first bitmap byte = 0x%02x, want bits 0..2 set", buf[0])
	}
}

func TestReadBitmapBytes_SkipBeyondChain(t *testing.T) {
	// When the requested start byte is past the end of the bitmap
	// chain, readBitmapBytes returns the zero-filled out buffer
	// without erroring. Format's single-cluster bitmap is 4096 bytes
	// long; ask for byte 1 000 000 (which sits after the chain ends).
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	buf, err := fs.readBitmapBytes(1_000_000, 4)
	if err != nil {
		t.Fatalf("readBitmapBytes past end: %v", err)
	}
	for i, b := range buf {
		if b != 0 {
			t.Errorf("expected zero byte at offset %d, got 0x%02x", i, b)
		}
	}
}

func TestReadBitmapBytes_FATReadFailsDuringSkip(t *testing.T) {
	// Force the FAT read during the skip-clusters loop to fail.
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	wrap := swapInFailingRW(fs, 0, 0)
	wrap.failReadAt = wrap.readCalls + 1
	clusterSize := uint64(fs.info.ClusterSize())
	if _, err := fs.readBitmapBytes(clusterSize+1, 1); err == nil {
		t.Error("readBitmapBytes should fail when FAT read fails during skip")
	}
}

// ── assertNoAllocBeyondViaBitmap edge branches ───────────────────────────

func TestAssertNoAllocBeyond_NewCountCoversAllBits(t *testing.T) {
	// When firstForbiddenBit >= totalBits, the function should return
	// nil immediately without scanning anything.
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	if err := fs.assertNoAllocBeyondViaBitmap(fs.info.ClusterCount); err != nil {
		t.Errorf("assertNoAllocBeyondViaBitmap with newCount>=total = %v", err)
	}
}

func TestAssertNoAllocBeyond_BitmapLengthLargerThanExpected(t *testing.T) {
	// When fs.bitmapLength is larger than ⌈ClusterCount/8⌉, the
	// function clamps it (line 226). Lying about bitmapLength forces
	// that path.
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	fs.bitmapLength = 99999 // way more than ClusterCount/8
	if err := fs.assertNoAllocBeyondViaBitmap(3); err != nil {
		t.Errorf("clamp-path = %v", err)
	}
}

func TestAssertNoAllocBeyond_StartByteAtOrPastEnd(t *testing.T) {
	// Construct a case where startByte == endByte so the function
	// returns nil before any reads. This happens when newClusterCount
	// equals ClusterCount - some_bit_offset and bitmapLength masks it.
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	// Pretend bitmapLength is exactly enough for newClusterCount bits.
	fs.bitmapLength = 1
	// newClusterCount == 8 → startByte = 1, endByte clamped to ⌈8/8⌉=1.
	// (We rig totalBits via fs.info.ClusterCount lying — but that risks
	// other branches; instead just pass a count whose startByte >= endByte.)
	if err := fs.assertNoAllocBeyondViaBitmap(8); err != nil {
		t.Errorf("startByte>=endByte path = %v", err)
	}
}

// ── readBitmapBytes more branches ────────────────────────────────────────

func TestReadBitmapBytes_SkipPastFullCluster(t *testing.T) {
	// Chain bitmap cluster 2 → cluster 5 → EOC, then ask for a read
	// whose start byte sits in cluster 5. This exercises the
	// "advance the chain" branch of the skip loop.
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	const tailCluster = uint32(5)
	if err := fs.setFATEntry(fs.bitmapCluster, tailCluster); err != nil {
		t.Fatalf("link bitmap: %v", err)
	}
	if err := fs.setFATEntry(tailCluster, 0xFFFFFFFF); err != nil {
		t.Fatalf("set EOC: %v", err)
	}
	clusterSize := uint64(fs.info.ClusterSize())
	buf, err := fs.readBitmapBytes(clusterSize+10, 5)
	if err != nil {
		t.Fatalf("readBitmapBytes(>clusterSize, 5): %v", err)
	}
	if len(buf) != 5 {
		t.Errorf("buf len = %d, want 5", len(buf))
	}
}

func TestReadBitmapBytes_DataReadFails(t *testing.T) {
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	// The first ReadAt issued by readBitmapBytes for a non-skipping
	// case is the data read.
	wrap := swapInFailingRW(fs, 0, 0)
	wrap.failReadAt = wrap.readCalls + 1
	if _, err := fs.readBitmapBytes(0, 1); err == nil {
		t.Error("readBitmapBytes should fail when data ReadAt fails")
	}
}

func TestReadBitmapBytes_ChainTerminatesEarlyInInnerLoop(t *testing.T) {
	// Chain bitmap 2 → 5, but cluster 5's FAT entry points to 0
	// (free) instead of EOC. The inner loop should detect that and
	// break cleanly. Reading enough to require entering cluster 5
	// triggers the branch.
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	const tailCluster = uint32(5)
	if err := fs.setFATEntry(fs.bitmapCluster, tailCluster); err != nil {
		t.Fatalf("link bitmap: %v", err)
	}
	if err := fs.setFATEntry(tailCluster, 0); err != nil {
		t.Fatalf("set tail to 0: %v", err)
	}
	clusterSize := uint64(fs.info.ClusterSize())
	// Request the last byte of cluster 2 plus 2 more bytes from
	// cluster 5. After reading the cluster-2 segment, the inner loop
	// advances and finds cluster 5's FAT entry = 0 → breaks.
	if _, err := fs.readBitmapBytes(clusterSize-1, 3); err != nil {
		t.Fatalf("readBitmapBytes spanning with bad tail: %v", err)
	}
}

func TestReadBitmapBytes_SpanningMultipleClusters(t *testing.T) {
	// Chain bitmap cluster 2 → cluster 5 → EOC. The first byte of
	// cluster 5 happens to be 0x00 (fresh image). Read a span that
	// straddles the cluster boundary; verify the read succeeds and
	// returns clusterSize+1 bytes (first cluster is mostly 0 except
	// bits for the system files at offset 0).
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	const tailCluster = uint32(5)
	if err := fs.setFATEntry(fs.bitmapCluster, tailCluster); err != nil {
		t.Fatalf("link bitmap: %v", err)
	}
	if err := fs.setFATEntry(tailCluster, 0xFFFFFFFF); err != nil {
		t.Fatalf("set EOC: %v", err)
	}
	clusterSize := uint64(fs.info.ClusterSize())
	buf, err := fs.readBitmapBytes(clusterSize-1, 3)
	if err != nil {
		t.Fatalf("readBitmapBytes spanning: %v", err)
	}
	if len(buf) != 3 {
		t.Errorf("buf size = %d, want 3", len(buf))
	}
}

func TestReadBitmapBytes_FATReadFailsDuringInnerLoop(t *testing.T) {
	// Chain bitmap so it crosses cluster boundaries, then fail the
	// FAT-read that advances the chain inside the inner loop.
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	const tailCluster = uint32(5)
	if err := fs.setFATEntry(fs.bitmapCluster, tailCluster); err != nil {
		t.Fatalf("link bitmap: %v", err)
	}
	if err := fs.setFATEntry(tailCluster, 0xFFFFFFFF); err != nil {
		t.Fatalf("set EOC: %v", err)
	}
	clusterSize := uint64(fs.info.ClusterSize())
	wrap := swapInFailingRW(fs, 0, 0)
	// First ReadAt of the inner loop is the bitmap-data read; the
	// second is the FAT chain advance. We let the data read succeed
	// and fail the FAT-advance.
	wrap.failReadAt = wrap.readCalls + 2
	if _, err := fs.readBitmapBytes(clusterSize-1, 2); err == nil {
		t.Error("readBitmapBytes should fail when FAT advance fails")
	}
}

// ── updateBitmapHeader: skip non-0x81 entries before reaching the bitmap ─

func TestUpdateBitmapHeader_SkipsNonBitmapEntries(t *testing.T) {
	// The format-default root layout puts the bitmap as the FIRST
	// entry. To hit the "skip non-bitmap" branch we synthesise a
	// preceding entry: e.g. invoke SetLabel which moves the bitmap
	// down — actually SetLabel writes the label *at* the bitmap slot
	// (the first non-EOD slot) which overwrites the bitmap (!) — so
	// instead, hand-craft a benign 0x40 entry ahead of the bitmap in
	// memory.
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	rootBuf, err := fs.readDirBuf(fs.info.RootDirectoryCluster)
	if err != nil {
		t.Fatalf("readDirBuf: %v", err)
	}
	// Reorder: move bitmap (offset 0) to offset dirEntrySize, write
	// a benign no-op entry at offset 0.
	bitmapEntry := make([]byte, dirEntrySize)
	copy(bitmapEntry, rootBuf[:dirEntrySize])
	// 0x40 — Allocation Possible Stream-like Volume GUID slot; not a
	// recognised entry, will be skipped.
	rootBuf[0] = 0x40
	for i := 1; i < dirEntrySize; i++ {
		rootBuf[i] = 0
	}
	copy(rootBuf[dirEntrySize:2*dirEntrySize], bitmapEntry)
	if err := fs.writeDirBuf(fs.info.RootDirectoryCluster, rootBuf); err != nil {
		t.Fatalf("writeDirBuf: %v", err)
	}
	// Now updateBitmapHeader must walk past the 0x40 entry and patch
	// the bitmap entry that follows.
	if err := fs.updateBitmapHeader(1234); err != nil {
		t.Fatalf("updateBitmapHeader: %v", err)
	}
}

func TestGrow_RefusesWhenBitmapTooSmall(t *testing.T) {
	// Real-world bitmap-cap test: format big enough that the FAT
	// covers much more than the bitmap, then ask for a grow that fits
	// in the FAT but blows past the bitmap. With a 16 MiB starting
	// image: fmtFATGrowthHeadroom=4 → FAT covers ~64 MiB worth of
	// clusters; the 1-cluster bitmap caps at 32768 bits → 128 MiB,
	// so we need a starting size where FAT capacity > 128 MiB. Use
	// 64 MiB — FAT capacity ≈ 256 MiB > bitmap ceiling 128 MiB.
	if testing.Short() {
		t.Skip("bitmap-cap test allocates a 64 MiB temp file")
	}
	const startSize = int64(64 * 1024 * 1024)
	fs, _ := freshFormattedFS(t, startSize)
	// Bitmap is 1 cluster of 4096 B = 32768 bits → 32768 clusters.
	// Compute the first size that needs more bits than that.
	clusterSize := int64(fs.info.ClusterSize())
	heapOffsetBytes := int64(fs.info.ClusterHeapOffset) * int64(fs.info.BytesPerSector())
	// Aim for newClusterCount = 32769 → > 32768.
	want := heapOffsetBytes + 32769*clusterSize
	if want <= startSize {
		t.Skip("starting image already exceeds the bitmap-cap threshold")
	}
	if err := fs.Grow(want); err == nil {
		t.Error("Grow past bitmap capacity should fail")
	} else if !strings.Contains(err.Error(), "bitmap") {
		t.Errorf("error %q does not mention bitmap", err)
	}
}

func TestUpdateBitmapHeader_BitmapEntryNotInRoot(t *testing.T) {
	// Stash bitmapCluster = 2 (looks like a real bitmap) but blast the
	// root directory's first entries so the loop walks past the
	// expected slot, hits 0x00 (EOD) and returns nil without writing.
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	rootBuf, err := fs.readDirBuf(fs.info.RootDirectoryCluster)
	if err != nil {
		t.Fatalf("readDirBuf: %v", err)
	}
	for i := range rootBuf[:128] {
		rootBuf[i] = 0
	}
	if err := fs.writeDirBuf(fs.info.RootDirectoryCluster, rootBuf); err != nil {
		t.Fatalf("writeDirBuf: %v", err)
	}
	// Now updateBitmapHeader should walk into an EOD at offset 0 and
	// return nil cleanly.
	if err := fs.updateBitmapHeader(1234); err != nil {
		t.Fatalf("updateBitmapHeader with EOD-only root = %v", err)
	}
}

func TestUpdateBitmapHeader_NoBitmapEntryButPaddedRoot(t *testing.T) {
	// Construct a root where every entry is non-bitmap, non-EOD so
	// the loop runs to completion and falls through to the trailing
	// return nil at the end of the function.
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	rootBuf, err := fs.readDirBuf(fs.info.RootDirectoryCluster)
	if err != nil {
		t.Fatalf("readDirBuf: %v", err)
	}
	// Fill with 0xC1 entries (Name entry — high bit set so they're
	// "in use" yet not 0x81). The function will iterate over every
	// 32-byte slot without finding a match.
	for i := range rootBuf {
		rootBuf[i] = 0xC1
	}
	if err := fs.writeDirBuf(fs.info.RootDirectoryCluster, rootBuf); err != nil {
		t.Fatalf("writeDirBuf: %v", err)
	}
	if err := fs.updateBitmapHeader(1234); err != nil {
		t.Fatalf("updateBitmapHeader fall-through = %v", err)
	}
}

func TestUpdateBitmapHeader_ReadDirFails(t *testing.T) {
	fs, _ := freshFormattedFS(t, 4*1024*1024)
	wrap := swapInFailingRW(fs, 0, 0)
	wrap.failReadAt = wrap.readCalls + 1
	if err := fs.updateBitmapHeader(123); err == nil {
		t.Error("updateBitmapHeader should fail when readDirBuf fails")
	}
}

func TestShrink_RefusesWhenRootClusterIsOutOfRange(t *testing.T) {
	fs, _ := freshFormattedFS(t, 8*1024*1024)
	// Lying about the root cluster forces the explicit root-out-of-range
	// branch in resize().
	fs.info.RootDirectoryCluster = fs.info.ClusterCount + 1
	// Try to shrink so the new clusterCount is below the (lying) root.
	if err := fs.Shrink(4 * 1024 * 1024); err == nil {
		t.Error("Shrink should fail when root cluster is beyond new range")
	} else if !strings.Contains(err.Error(), "root directory cluster") {
		t.Errorf("error %q does not mention root cluster", err)
	}
}
