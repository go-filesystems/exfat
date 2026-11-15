package filesystem_exfat

// The Linux kernel and exfatprogs as the judge of this driver, in a Tart VM.
// Skipped unless FS_KERNEL_JUDGE=1; github.com/go-filesystems/kerneljudge's
// cmd/kerneljudge sets it and runs this as root in the VM:
//
//	kerneljudge run --driver <clone of this repository> [--perf]

import (
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
		Open: func(img string) (filesystem.Filesystem, error) {
			return Open(img, -1)
		},
		Format: func(img string, size int64) (filesystem.Filesystem, error) {
			return Format(img, size, FormatConfig{})
		},
		Mkfs:    []string{"mkfs.exfat", "{img}"},
		Fsck:    [][]string{{"fsck.exfat", "-n", "{img}"}},
		FSCaps:  &caps,
		Compare: kj.Compare{Mtime: true, MtimeTolerance: 2 * time.Second},
	}
}

func TestKernelJudge(t *testing.T)     { kj.Run(t, kernelJudgeDriver()) }
func TestKernelJudgePerf(t *testing.T) { kj.RunPerf(t, kernelJudgeDriver(), kj.PerfOptions{}) }
