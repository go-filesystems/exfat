<p align="center"><img src="https://raw.githubusercontent.com/go-filesystems/brand/main/social/go-filesystems-exfat.png" alt="go-filesystems/exfat" width="720"></p>

# exfat

[![Go Reference](https://pkg.go.dev/badge/github.com/go-filesystems/exfat.svg)](https://pkg.go.dev/github.com/go-filesystems/exfat)
[![License: BSD-3-Clause](https://img.shields.io/badge/License-BSD%203--Clause-blue.svg)](https://opensource.org/licenses/BSD-3-Clause)
[![CI](https://github.com/go-filesystems/exfat/actions/workflows/ci.yml/badge.svg)](https://github.com/go-filesystems/exfat/actions/workflows/ci.yml)

Pure-Go read/write access to exFAT filesystem images — no root privileges, no external tools, no CGO.

Supports bare filesystem images and MBR/GPT partitioned disks, full directory traversal, file mutation and filesystem creation.

## Support summary

| Feature | Status | Notes |
|---|---:|---|
| Open / Close | ✅ | Supports bare images and partitioned disks |
| Format | ✅ | Creates exFAT images |
| ReadFile | ✅ | Full file reads supported |
| Read at an offset | ✅ | `OpenFile(path)` → `io.ReaderAt` + `Size()` (`filesystem.Opener` / `filesystem.File`) |
| WriteFile | ✅ | Full file writes supported |
| MkDir / Delete / Rename | ✅ | Directory operations supported |
| ReadLink / Symlinks | ⚠️ No | exFAT does not support POSIX symlinks |
| Volume label | ✅ | `Label` / `SetLabel` (`filesystem.Labeller`) |
| Truncate | ✅ | `Truncate(path, newSize)` (`filesystem.Truncater`) |
| Grow / Shrink / Resize | ✅ | Format provisions FAT headroom so `Grow` doesn't immediately need relocation |
| Partitioned images | ✅ | MBR/GPT supported |

## Limitations

- exFAT does not support POSIX symlinks or POSIX permissions/ACLs.
- Metadata is limited compared to POSIX filesystems (no ownership, no Unix permissions).
- No journaling; this implementation is intended for tooling and tests, not production workloads.

## Module

```text
github.com/go-filesystems/exfat
```

## Supported operations

| Operation    | Status         |
|--------------|----------------|
| Open / Close | ✅ implemented |
| Format       | ✅ implemented |
| Stat         | ✅ implemented |
| ListDir      | ✅ implemented |
| ReadFile     | ✅ implemented |
| OpenFile     | ✅ implemented (`filesystem.Opener`) |
| WriteFile    | ✅ implemented |
| MkDir        | ✅ implemented |
| DeleteFile   | ✅ implemented |
| DeleteDir    | ✅ implemented (recursive) |
| Rename       | ✅ implemented |
| ReadLink     | ⚠️ stub — exFAT has no symlinks |

## API

`Open` and `Format` return the plain `filesystem.Filesystem` interface (there
is no richer exported `FS` type) — exFAT-specific extras (`Info`,
`PartitionOffset`, volume label, truncate, resize) are reached by
type-asserting to the optional interfaces from
`github.com/go-filesystems/interface`, or to the package's own `Info` getter.

### Format / Open

```go
type FormatConfig struct {
    Label              string
    VolumeSerialNumber uint32 // 0 = randomly generated
}

func Format(path string, sizeBytes int64, cfg FormatConfig) (filesystem.Filesystem, error)
func Open(imagePath string, partIndex int) (filesystem.Filesystem, error)
```

### Read / Write

The returned value's `Stat` / `ListDir` / `ReadFile` / `WriteFile` / `MkDir` /
`DeleteFile` / `DeleteDir` / `Rename` are exactly the
[`filesystem.Filesystem`](https://github.com/go-filesystems/interface)
contract — see that package's README for the full signatures.

### exFAT-specific extras (type-assert)

```go
// Read part of a file without materialising all of it: OpenFile resolves the
// FAT chain once (cluster numbers, not data) and then serves byte ranges, so a
// 4 KiB read out of a 4 GiB file costs 4 KiB. Required by anything that mounts
// or exports the volume.
if o, ok := fs.(filesystem.Opener); ok {
    f, err := o.OpenFile("/big.bin")
    if err != nil {
        return err
    }
    defer f.Close()
    buf := make([]byte, 4096)
    n, err := f.ReadAt(buf, 1<<30) // io.ReaderAt semantics, exactly
    _, _ = n, err
    _ = f.Size()                   // from metadata; reads nothing
}
if l, ok := fs.(filesystem.Labeller); ok {
    _ = l.SetLabel("MYVOL")
}
if t, ok := fs.(filesystem.Truncater); ok {
    _ = t.Truncate("/big.bin", 4096)
}
if r, ok := fs.(interface{ Resize(int64) error }); ok {
    _ = r.Resize(newSizeBytes) // also exposes Grow(int64) error / Shrink(int64) error
}
if i, ok := fs.(interface{ Info() Info }); ok {
    fmt.Println(i.Info().ClusterSize())
}
```

## Implements

This package implements the `filesystem.Filesystem` interface defined in
`github.com/go-filesystems/interface`. Callers can treat the value returned
by `Open`/`Format` as a `filesystem.Filesystem` to write generic tooling that
works across the other filesystem modules in this repository.

Example:

```go
import (
    filesystem "github.com/go-filesystems/interface"
    fsex "github.com/go-filesystems/exfat"
)

f, _ := fsex.Open("exfat.img", -1)
defer f.Close()
var fs filesystem.Filesystem = f
_, _ = fs.ReadFile("/hello.txt")
```
