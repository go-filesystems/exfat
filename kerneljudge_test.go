package filesystem_exfat

// The Linux kernel and exfatprogs as the judge of this driver, in a Tart VM.
// Skipped unless FS_KERNEL_JUDGE=1; github.com/go-filesystems/kerneljudge's
// cmd/kerneljudge sets it and runs this as root in the VM:
//
//	kerneljudge run --driver <clone of this repository> [--perf]

import (
	"strings"
	"testing"
	"time"

	filesystem "github.com/go-filesystems/interface"
	kj "github.com/go-filesystems/kerneljudge"
)

func kernelJudgeDriver() kj.Driver {
	// exFAT has no symlinks, no extended attributes and no permission bits (the
	// kernel mounts every file with the same fmask), and times are 10 ms with a
	// two-second access time.
	caps := kj.Caps{Sparse: true, Mtime: true}
	return kj.Driver{
		Name:   "exfat",
		FSType: "exfat",
		// Format refuses an image over 128 MiB ("bitmap size ... exceeds one
		// cluster"); the kernel's mkfs.exfat has no such limit.
		ImageSize: 128 << 20,
		Open: func(img string) (filesystem.Filesystem, error) {
			return Open(img, -1)
		},
		Format: func(img string, size int64) (filesystem.Filesystem, error) {
			return Format(img, size, FormatConfig{})
		},
		// Defects that hide the rest of an image get a variant without them:
		// a name with a non-ASCII Latin letter (wrong name hash), and a
		// directory (written with a zero length: the kernel sees it empty).
		Variants: []kj.Variant{
			{
				Name: "ascii-latin",
				Map: func(e kj.Entry) (kj.Entry, bool) {
					e.Path = strings.ReplaceAll(e.Path, "é", "e")
					return e, true
				},
			},
			{
				Name: "root-files",
				Map: func(e kj.Entry) (kj.Entry, bool) {
					return e, e.Kind == kj.File && strings.Count(e.Path, "/") == 1
				},
			},
		},
		Known: []kj.Known{
			{Test: `/write$`, Match: `^fsck fsck.exfat`, Issue: "exfat#35, #36"},
			{Test: `/write-ascii-latin$`, Match: `^fsck fsck.exfat`, Issue: "exfat#35"},
			{Test: `/write(-ascii-latin)?$`, Match: `^kernel mount`, Issue: "exfat#35, #36"},
		},
		Mkfs:    []string{"mkfs.exfat", "{img}"},
		Fsck:    [][]string{{"fsck.exfat", "-n", "{img}"}},
		FSCaps:  &caps,
		Compare: kj.Compare{Mtime: true, MtimeTolerance: 2 * time.Second},
	}
}

func TestKernelJudge(t *testing.T)     { kj.Run(t, kernelJudgeDriver()) }
func TestKernelJudgePerf(t *testing.T) { kj.RunPerf(t, kernelJudgeDriver(), kj.PerfOptions{}) }
