# Performance benchmarks

Two halves that measure the **same standard operations** so the pure-Go exFAT
driver can be read side by side with the in-kernel exFAT implementation.

## Go-driver side (portable, runs anywhere)

```sh
GOWORK=off go test -bench=. -benchmem -run='^$'
```

Benchmarks (in `../bench_test.go`, public-API only): `Format`, `WriteFileSeq`,
`ReadFileSeq`, `Stat`, `ListDir`, `CreateFiles`, `DeleteFiles`. A file-backed
image under `b.TempDir()` is used so the numbers include real block I/O.

The image is 64 MiB: exFAT `Format` currently supports images up to ~128 MiB,
and 64 MiB leaves comfortable room for the large-file and many-file cases.

## Reference side (in-kernel exFAT, Linux only, needs root)

```sh
scp bench/compare.sh dc1-r1-h1:/tmp/ && ssh dc1-r1-h1 'sudo bash /tmp/compare.sh'
```

`compare.sh` runs the same ops via `mkfs.exfat` + `mount -o loop` + `dd`
(with `fsync`/`drop_caches`) + coreutils.

> **Caveat — not apples-to-apples.** The kernel has a page cache and writeback;
> the Go driver does synchronous user-space block I/O. Treat the kernel numbers
> as a rough upper-bound reference, not a literal target.

## First findings (2026-06, Apple M4 Max, 64 MiB image)

Go-driver side, `-benchtime=3x`:

| Operation        | go-filesystems/exfat            |
|------------------|---------------------------------|
| Format           | ~0.97 ms/op                     |
| Sequential read  | ~893 MB/s (~9.4 ms for 8 MiB)   |
| Sequential write | ~6.3 MB/s (~1.33 s for 8 MiB)   |
| Stat             | ~0.74 ms/op                     |
| ListDir (200)    | ~0.65 ms/op                     |
| Create file      | ~0.27 ms/file (~54.9 ms / 200)  |
| Delete file      | ~0.26 ms/file (~52.4 ms / 200)  |

**Reads are fast; sequential write is the clear outlier** — ~6 MB/s and
allocation-heavy. As with the ext4 pilot, the cost is almost certainly
per-operation metadata rewrites (allocation bitmap / FAT / directory entries)
instead of batching dirty clusters — an algorithmic write-path issue,
identical across all target architectures (no SIMD involved).

This is the top optimization target; profile with
`GOWORK=off go test -bench=BenchmarkWriteFileSeq -cpuprofile=cpu.out -memprofile=mem.out`.
