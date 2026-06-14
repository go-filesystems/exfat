#!/usr/bin/env bash
#
# compare.sh — reference (in-kernel exFAT + coreutils) throughput for the same
# standard operations the Go benchmarks (bench_test.go) measure, so the two can
# be read side by side.
#
# This is the "reference" half of the perf comparison. It must run on Linux as
# root (loop mount, exfat kernel module + exfatprogs); on the weft VMs:
#
#     scp bench/compare.sh dc1-r1-h1:/tmp/ && ssh dc1-r1-h1 'sudo bash /tmp/compare.sh'
#
# Caveat: the kernel exFAT driver has the page cache and writeback; the Go
# driver does synchronous block I/O in user space. These numbers are a rough
# upper-bound reference, not an apples-to-apples target.
set -euo pipefail
export PATH=/usr/local/sbin:/usr/sbin:/sbin:$PATH

IMG=${IMG:-/tmp/exfat-ref.img}
MNT=${MNT:-/tmp/exfat-ref-mnt}
SIZE_MB=${SIZE_MB:-64}
BIG_MB=${BIG_MB:-8}
NFILES=${NFILES:-200}

cleanup() { umount "$MNT" 2>/dev/null || true; rm -f "$IMG"; }
trap cleanup EXIT
mkdir -p "$MNT"

ms() { date +%s%3N; }
report() { printf '%-22s %s\n' "$1" "$2"; }

echo "== reference: in-kernel exFAT (exfatprogs + coreutils) =="
echo "image=${SIZE_MB}MiB bigfile=${BIG_MB}MiB nfiles=${NFILES}"
echo

# --- Format (mkfs.exfat) ---
rm -f "$IMG"; truncate -s "${SIZE_MB}M" "$IMG"
t0=$(ms); mkfs.exfat "$IMG" >/dev/null 2>&1; t1=$(ms)
report "Format (mkfs.exfat)" "$((t1 - t0)) ms"

mount -o loop "$IMG" "$MNT"

# --- Sequential write (dd + fsync) ---
sync; echo 3 > /proc/sys/vm/drop_caches 2>/dev/null || true
dd if=/dev/zero of="$MNT/big.bin" bs=1M count="$BIG_MB" conv=fsync 2>/tmp/dd.w
report "WriteFileSeq (dd)" "$(grep -oE '[0-9.]+ [MG]B/s' /tmp/dd.w | tail -1)"

# --- Sequential read (drop caches first) ---
sync; echo 3 > /proc/sys/vm/drop_caches 2>/dev/null || true
dd if="$MNT/big.bin" of=/dev/null bs=1M 2>/tmp/dd.r
report "ReadFileSeq (dd)" "$(grep -oE '[0-9.]+ [MG]B/s' /tmp/dd.r | tail -1)"

# --- Create many files ---
mkdir -p "$MNT/d"
t0=$(ms); for i in $(seq 1 "$NFILES"); do : > "$MNT/d/f$i"; done; sync; t1=$(ms)
report "CreateFiles ($NFILES)" "$((t1 - t0)) ms"

# --- ListDir ---
t0=$(ms); for r in $(seq 1 50); do ls -f "$MNT/d" >/dev/null; done; t1=$(ms)
report "ListDir (x50)" "$((t1 - t0)) ms"

# --- Stat ---
t0=$(ms); for r in $(seq 1 1000); do stat "$MNT/d/f1" >/dev/null; done; t1=$(ms)
report "Stat (x1000)" "$((t1 - t0)) ms"

# --- Delete ---
t0=$(ms); for i in $(seq 1 "$NFILES"); do rm "$MNT/d/f$i"; done; sync; t1=$(ms)
report "DeleteFiles ($NFILES)" "$((t1 - t0)) ms"

echo
echo "Run the Go-driver side with:  go test -bench=. -benchmem -run='^\$'"
