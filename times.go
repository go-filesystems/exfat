package filesystem_exfat

import (
	"encoding/binary"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

var _ filesystem.TimesReader = (*exfatFS)(nil)

// Resolutions of the exFAT times (Microsoft exFAT specification, 7.4.8-7.4.9):
// a Timestamp holds two-second multiples; the create and modify times carry a
// 10msIncrement byte, the access time does not.
const (
	exfatTimestampRes = 2 * time.Second
	exfatIncrementRes = 10 * time.Millisecond
)

// Times returns the times recorded in the File directory entry of path.
//
//   - Btime is CreateTimestamp + Create10msIncrement, resolution 10 ms.
//   - Mtime is LastModifiedTimestamp + LastModified10msIncrement, 10 ms.
//   - Atime is LastAccessedTimestamp, resolution 2 s (it has no increment).
//   - Ctime is not recorded: exFAT has no change time.
//
// Each time is local date-and-time plus its own UtcOffset byte. When
// OffsetValid (bit 7) is set the 7-bit two's-complement OffsetFromUtc, in
// 15-minute steps, gives the zone. When it is clear the spec says to treat the
// time as having the current local offset, so it is read in time.Local. A
// field whose date does not exist (month 0, day 0, 31 April, hour 24, ...) or
// whose increment is above 199 is reported as not recorded, never normalised.
// The root directory has no entry, so none of its times are recorded.
func (fs *exfatFS) Times(path string) (filesystem.FileTimes, error) {
	e, _, err := fs.resolvePath(path)
	if err != nil {
		return filesystem.FileTimes{}, err
	}
	if len(pathComponents(path)) == 0 {
		return filesystem.FileTimes{}, nil
	}
	return decodeExfatTimes(e.times[:]), nil
}

// decodeExfatTimes decodes the 17 bytes at offsets 8..24 of a File entry.
func decodeExfatTimes(b []byte) filesystem.FileTimes {
	le := binary.LittleEndian
	return filesystem.FileTimes{
		Btime: decodeExfatTime(le.Uint32(b[0:]), b[12], b[14], exfatIncrementRes),
		Mtime: decodeExfatTime(le.Uint32(b[4:]), b[13], b[15], exfatIncrementRes),
		Atime: decodeExfatTime(le.Uint32(b[8:]), 0, b[16], exfatTimestampRes),
	}
}

// decodeExfatTime decodes one Timestamp, its 10msIncrement and UtcOffset byte.
// An all-zero timestamp is not a date (month 0) and so is not recorded.
func decodeExfatTime(ts uint32, inc, off uint8, res time.Duration) filesystem.Timestamp {
	year := 1980 + int(ts>>25)
	month := int(ts>>21) & 0x0F
	day := int(ts>>16) & 0x1F
	hour := int(ts>>11) & 0x1F
	min := int(ts>>5) & 0x3F
	sec := int(ts&0x1F) * 2
	if month < 1 || month > 12 || day < 1 || hour > 23 || min > 59 || sec > 58 || inc > 199 {
		return filesystem.Timestamp{}
	}
	if day > time.Date(year, time.Month(month)+1, 0, 0, 0, 0, 0, time.UTC).Day() {
		return filesystem.Timestamp{}
	}
	sec += int(inc) / 100
	ns := (int(inc) % 100) * 10_000_000
	loc := time.Local
	if off&0x80 != 0 {
		// 7-bit two's complement, units of 15 minutes.
		steps := int(off & 0x7F)
		if steps >= 0x40 {
			steps -= 0x80
		}
		loc = time.FixedZone("", steps*15*60)
	}
	return filesystem.NewTimestamp(time.Date(year, time.Month(month), day, hour, min, sec, ns, loc), res)
}
