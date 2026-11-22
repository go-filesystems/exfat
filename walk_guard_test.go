package filesystem_exfat

import (
	"errors"
	"testing"

	"github.com/go-volumes/safeio"
)

// An unreadable directory is an error of the check, not a pass.
func TestCheckDirTreeReportsUnreadableDirectory(t *testing.T) {
	path := exfatTestImage(t, func(root []byte) {
		root[0] = exfatEntryEnd
	}, map[uint32]uint32{}, map[uint32][]byte{})
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()
	w := fs.(*exfatFS)
	if err := w.checkDirTree(0x7FFFFFF0); err == nil {
		t.Fatal("checkDirTree of a cluster outside the volume = nil, want an error")
	}
}

// TestDeleteDirCycleIsRefused: a directory whose child entry names the
// directory's OWN first cluster used to make DeleteDir recurse until the
// goroutine stack overflowed (a fatal error, no recover()). It is now refused
// before anything is freed.
func TestDeleteDirCycleIsRefused(t *testing.T) {
	loop := make([]byte, 4096)
	writeExFATEntrySetWithAttr(loop[0:], "self", 4, exfatAttrDir, 0)
	loop[96] = exfatEntryEnd

	path := exfatTestImage(t, func(root []byte) {
		writeExFATEntrySetWithAttr(root[0:], "loop", 4, exfatAttrDir, 0)
		root[96] = exfatEntryEnd
	}, map[uint32]uint32{4: 0xFFFFFFFF}, map[uint32][]byte{4: loop})

	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	if _, err := fs.ListDir("/loop"); err != nil {
		t.Fatalf("ListDir(/loop): %v", err)
	}
	err = fs.DeleteDir("/loop")
	if !errors.Is(err, safeio.ErrCycle) {
		t.Fatalf("DeleteDir(/loop) = %v, want safeio.ErrCycle", err)
	}
	// Nothing was removed: the entry is still listed.
	if _, err := fs.ListDir("/loop"); err != nil {
		t.Fatalf("directory gone after a refused delete: %v", err)
	}
}
