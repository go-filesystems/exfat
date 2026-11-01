package filesystem_exfat

import "testing"

func TestBomb_ProbeSize(t *testing.T) {
	for _, size := range []int{0, 1, 2, 3, 511, 512, 513, 4096, 65537} {
		if got := probeSize(&mockDisk{data: make([]byte, size)}); got != int64(size) {
			t.Errorf("probeSize(%d) = %d", size, got)
		}
	}
}

func TestBomb_ReadBackedIsExact(t *testing.T) {
	d := &mockDisk{data: make([]byte, 1000)}
	if b, err := readBacked(d, 900, 100); err != nil || len(b) != 100 {
		t.Errorf("up to the last byte: len=%d err=%v", len(b), err)
	}
	if _, err := readBacked(d, 900, 101); err == nil {
		t.Error("one byte past the end was read")
	}
	if _, err := readBacked(d, -1, 4); err == nil {
		t.Error("negative offset accepted")
	}
	if _, err := readBacked(d, 1<<62, 1<<62); err == nil {
		t.Error("overflowing range accepted")
	}
	if b, err := readBacked(d, 0, 0); err != nil || len(b) != 0 {
		t.Errorf("empty read: len=%d err=%v", len(b), err)
	}
}
