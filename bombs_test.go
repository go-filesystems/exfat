package filesystem_exfat

// Memory-bomb regression tests. Each hands the driver a few hundred bytes of
// image that claim gigabytes, and asserts the read is refused or shortened and
// that less than bombBudget was allocated doing it (runtime.MemStats
// TotalAlloc before/after).

import (
	"encoding/binary"
	"runtime"
	"testing"
)

const bombBudget = 1 << 20

func totalAlloc(f func()) uint64 {
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	f()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}

// bombBoot is defaultExFATBootSector with its geometry overridden.
func bombBoot(spcShift byte, clusterHeapOffset, clusterCount uint32) []byte {
	b := defaultExFATBootSector()
	b[109] = spcShift
	binary.LittleEndian.PutUint32(b[88:], clusterHeapOffset)
	binary.LittleEndian.PutUint32(b[92:], clusterCount)
	return b
}

// Open reads the root cluster twice (the allocation bitmap and the label). The
// cluster size comes from the boot sector -- 512 << 16 = 32 MiB -- and the
// buffer used to be made before the read could fail on an image that is 512
// bytes long.
func TestBomb_OpenDoesNotReserveAClusterTheImageLacks(t *testing.T) {
	boot := bombBoot(16, 280, 4096)
	n := totalAlloc(func() { _, _ = openAt(&mockDisk{data: boot}, 0) })
	if n >= bombBudget {
		t.Errorf("Open of a 512-byte image allocated %d bytes", n)
	}
}

func TestBomb_ReadClusterChainDoesNotReserveAClusterTheImageLacks(t *testing.T) {
	boot := bombBoot(16, 280, 4096)
	info, err := readInfo(&mockDisk{data: boot}, 0)
	if err != nil {
		t.Fatal(err)
	}
	image := make([]byte, 256<<10)
	copy(image, boot)
	fs := newMockFSWithErrors(image, info, 0, nil, nil)
	binary.LittleEndian.PutUint32(image[int(info.FATOffsetBytes(0))+2*4:], 0xFFFFFFFF)
	var rerr error
	n := totalAlloc(func() { _, rerr = fs.readClusterChain(2, 1<<40) })
	if rerr == nil {
		t.Error("a 32 MiB cluster in a 256 KiB image was read")
	}
	if n >= bombBudget {
		t.Errorf("allocated %d bytes before failing", n)
	}
}

// A boot sector that declares 4M clusters and a NoFatChain entry that declares
// them all: the cluster list (and a result buffer behind it) was sized by the
// two claims, not by the 64 KiB image.
func TestBomb_ContiguousRunBoundedByImage(t *testing.T) {
	boot := bombBoot(0, 8, 1<<22) // 512-byte clusters, heap at byte 4096
	info, err := readInfo(&mockDisk{data: boot}, 0)
	if err != nil {
		t.Fatal(err)
	}
	image := make([]byte, 64<<10)
	copy(image, boot)
	fs := newMockFSWithErrors(image, info, 0, nil, nil)
	var clusters []uint32
	var covered int64
	n := totalAlloc(func() { clusters, covered = fs.contiguousClusters(2, 1<<41) })
	if want := int64(len(image)) - 4096; covered != want {
		t.Errorf("covered %d bytes of a %d-byte heap", covered, want)
	}
	if len(clusters) != 120 {
		t.Errorf("%d clusters listed, want the 120 the image holds", len(clusters))
	}
	if n >= bombBudget {
		t.Errorf("allocated %d bytes", n)
	}
	n = totalAlloc(func() { _, _ = fs.readContiguousRun(2, 1<<41) })
	if n >= bombBudget {
		t.Errorf("readContiguousRun allocated %d bytes", n)
	}
}

// The run ends exactly where the image does: the last cluster is listed, one
// byte less of image and it is still listed (it is partly there), a heap
// entirely past the end lists nothing.
func TestBomb_ContiguousRunBoundIsExact(t *testing.T) {
	boot := bombBoot(0, 8, 1<<22)
	info, _ := readInfo(&mockDisk{data: boot}, 0)
	for _, tc := range []struct {
		image int
		start uint32
		want  int
	}{
		{4096 + 512, 2, 1},
		{4096 + 511, 2, 1},
		{4096, 2, 0},
		{4096 + 1024, 3, 1},
		{4096 + 1024, 4, 0},
	} {
		img := make([]byte, tc.image)
		fs := newMockFSWithErrors(img, info, 0, nil, nil)
		got, _ := fs.contiguousClusters(tc.start, 1<<41)
		if len(got) != tc.want {
			t.Errorf("image %d bytes, start %d: %d clusters, want %d", tc.image, tc.start, len(got), tc.want)
		}
	}
}

// The bitmap length is a claim in the root directory.
func TestBomb_BitmapReadBoundedByImage(t *testing.T) {
	boot := bombBoot(0, 8, 1<<22)
	info, _ := readInfo(&mockDisk{data: boot}, 0)
	fs := newMockFSWithErrors(make([]byte, 64<<10), info, 0, nil, nil)
	fs.bitmapCluster = 2
	var err error
	n := totalAlloc(func() { _, err = fs.readBitmapBytes(0, 1<<32) })
	if err == nil {
		t.Error("a 4 GiB bitmap was accepted from a 64 KiB image")
	}
	if n >= bombBudget {
		t.Errorf("allocated %d bytes", n)
	}
}
