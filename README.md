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
| WriteFile | ✅ | Full file writes supported |
| MkDir / Delete / Rename | ✅ | Directory operations supported |
| ReadLink / Symlinks | ⚠️ No | exFAT does not support POSIX symlinks |
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
| WriteFile    | ✅ implemented |
| MkDir        | ✅ implemented |
| DeleteFile   | ✅ implemented |
| DeleteDir    | ✅ implemented (recursive) |
| Rename       | ✅ implemented |
| ReadLink     | ⚠️ stub — exFAT has no symlinks |

## API

### Format

```go
type FormatConfig struct {
    Label              string
    VolumeSerialNumber uint32 // 0 = randomly generated
}

func Format(path string, sizeBytes int64, cfg FormatConfig) (*FS, error)
```

### Open

```go
func Open(imagePath string, partIndex int) (*FS, error)
func (fs *FS) Close() error
func (fs *FS) Info() Info
func (fs *FS) PartitionOffset() int64
```

### Read

```go
func (fs *FS) Stat(path string) (filesystem.Stat, error)
func (fs *FS) ListDir(path string) ([]filesystem.DirEntry, error)
func (fs *FS) ReadFile(path string) ([]byte, error)
```

### Write

```go
func (fs *FS) WriteFile(path string, data []byte, perm os.FileMode) error
func (fs *FS) MkDir(path string, perm os.FileMode) error
func (fs *FS) DeleteFile(path string) error
func (fs *FS) DeleteDir(path string) error
func (fs *FS) Rename(oldPath, newPath string) error
```

## Implements

This package implements the `filesystem.Filesystem` interface defined in
`github.com/go-filesystems/interface`. Callers can treat an opened `*FS`
as a `filesystem.Filesystem` to write generic tooling that works across the
other filesystem modules in this repository.

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
