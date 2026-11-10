package filesystem_exfat

import (
	"encoding/binary"
	"errors"
	"fmt"
	iofs "io/fs"
	"time"
)

// The driver has Chtimes but not Chmod or Chown (exFAT has no owner, and the
// attribute bits are a separate change), so it does not satisfy the bundled
// filesystem.MetadataSetter; callers probe for the single method.
var _ interface {
	Chtimes(path string, atime, mtime time.Time) error
} = (*exfatFS)(nil)

// ErrTimeOutOfRange is what Chtimes returns, wrapped in a *TimeRangeError, for
// a time the exFAT Timestamp field cannot hold: a 7-bit year counted from 1980,
// so the representable years are 1980..2107. The driver never clamps.
var ErrTimeOutOfRange = errors.New("exfat: time outside the exFAT range 1980-2107")

// TimeRangeError names the field and the value Chtimes refused.
type TimeRangeError struct {
	Field string // "atime" or "mtime"
	Time  time.Time
}

func (e *TimeRangeError) Error() string {
	return fmt.Sprintf("exfat: %s %s is outside the exFAT range 1980-2107", e.Field, e.Time.Format(time.RFC3339))
}

func (e *TimeRangeError) Unwrap() error { return ErrTimeOutOfRange }

// exfatEncodedTime is one time as the File directory entry stores it.
type exfatEncodedTime struct {
	ts  uint32 // Timestamp: date and time, 2 s units, in the zone of off
	inc uint8  // 10msIncrement, 0..199: the odd second and the 10 ms units
	off uint8  // UtcOffset: OffsetValid (0x80) | 7-bit two's complement, 15 min
}

// encodeExfatTime encodes t as the Timestamp, 10msIncrement and UtcOffset of
// Microsoft's exFAT specification, 7.4.8-7.4.10.
//
// The zone is a choice the specification leaves to the writer. The time is
// written as the wall clock of time.Local at that instant, with OffsetValid set
// and that zone's UtcOffset, so a reader that honours the offset recovers the
// exact instant and one that ignores it (the spec's reading of a clear
// OffsetValid) still shows the wall clock of the machine's own zone. When the
// local offset at t is not a whole number of 15-minute steps, or does not fit
// the 7-bit field (-16:00..+15:45), the time is written as UTC with offset 0
// instead, which is always representable.
//
// ok is false when the wall-clock year is outside 1980..2107.
func encodeExfatTime(t time.Time) (e exfatEncodedTime, ok bool) {
	w := t.In(time.Local)
	_, offSec := w.Zone()
	steps := offSec / 900
	if offSec%900 != 0 || steps < -64 || steps > 63 {
		w, steps = t.UTC(), 0
	}
	if w.Year() < 1980 || w.Year() > 1980+127 {
		return e, false
	}
	e.ts = uint32(w.Year()-1980)<<25 | uint32(w.Month())<<21 | uint32(w.Day())<<16 |
		uint32(w.Hour())<<11 | uint32(w.Minute())<<5 | uint32(w.Second()/2)
	e.inc = uint8((w.Second()%2)*100 + w.Nanosecond()/10_000_000)
	e.off = 0x80 | uint8(steps)&0x7F
	return e, true
}

// Chtimes sets the access and modification times of path (Microsoft, "exFAT
// file system specification", 7.4.5-7.4.10, the File directory entry).
//
//   - mtime is written to LastModifiedTimestamp, LastModified10msIncrement and
//     LastModifiedUtcOffset: resolution 10 ms, rounded down, as Times reports.
//   - atime is written to LastAccessedTimestamp and LastAccessedUtcOffset:
//     the field has no increment, so the resolution is 2 s, rounded down.
//   - The create time (and its increment and offset) is not touched, and exFAT
//     has no change time to refresh.
//
// See encodeExfatTime for the zone each time is written in. A zero time.Time
// leaves that field unchanged. A time whose wall-clock year is outside
// 1980..2107 is refused with a *TimeRangeError (errors.Is ErrTimeOutOfRange)
// before anything is written; neither field is changed then. The root
// directory has no entry, so it has no times to set.
//
// The File entry set's checksum (SetChecksum, over the File entry and all its
// secondaries) is recomputed and the directory is written back through
// writeDirBuf, like every other entry update, so a failed write leaves the old
// times and a valid set.
func (fs *exfatFS) Chtimes(path string, atime, mtime time.Time) error {
	var a, m exfatEncodedTime
	var ok bool
	if !atime.IsZero() {
		if a, ok = encodeExfatTime(atime); !ok {
			return &TimeRangeError{Field: "atime", Time: atime}
		}
	}
	if !mtime.IsZero() {
		if m, ok = encodeExfatTime(mtime); !ok {
			return &TimeRangeError{Field: "mtime", Time: mtime}
		}
	}
	name, parent, err := fs.getParentDir(path)
	if err != nil {
		return err
	}
	// getParentDir refuses "/" itself: the root directory has no entry.
	buf, err := fs.readDirBuf(parent)
	if err != nil {
		return err
	}
	off, secondaryCount := exfatFindEntry(buf, name)
	if off < 0 {
		return fmt.Errorf("exfat: %q not found: %w", path, iofs.ErrNotExist)
	}
	if atime.IsZero() && mtime.IsZero() {
		return nil
	}
	setLen := (secondaryCount + 1) * dirEntrySize
	e := buf[off : off+setLen]
	putExfatTimes(e, atime, a, mtime, m)
	binary.LittleEndian.PutUint16(e[2:4], exfatEntrySetChecksum(e))
	return fs.writeDirBuf(parent, buf)
}

// putExfatTimes stores the encoded times into the File entry e (offsets: modify
// timestamp 12, access timestamp 16, modify increment 21, modify offset 23,
// access offset 24), skipping a time whose source was zero.
func putExfatTimes(e []byte, atime time.Time, a exfatEncodedTime, mtime time.Time, m exfatEncodedTime) {
	le := binary.LittleEndian
	if !mtime.IsZero() {
		le.PutUint32(e[12:16], m.ts)
		e[21] = m.inc
		e[23] = m.off
	}
	if !atime.IsZero() {
		le.PutUint32(e[16:20], a.ts)
		e[24] = a.off
	}
}
