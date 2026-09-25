//go:build linux

package filesystem

import (
	"errors"
	"fmt"
	"path/filepath"
	"syscall"
	"unsafe"
)

const renameNoReplace = 1

// RenameNoReplace atomically renames oldPath to newPath only when newPath does
// not exist. It is the namespace primitive used for declaration relocation;
// aninode never implements no-replace as stat-then-rename.
func RenameNoReplace(oldPath, newPath string) error {
	nr, ok := renameat2SyscallNumber()
	if !ok {
		return errors.New("renameat2(RENAME_NOREPLACE) unsupported on this Linux architecture")
	}
	oldPtr, err := syscall.BytePtrFromString(oldPath)
	if err != nil {
		return err
	}
	newPtr, err := syscall.BytePtrFromString(newPath)
	if err != nil {
		return err
	}
	atFDCWD := ^uintptr(99) // -100 as uintptr
	_, _, errno := syscall.Syscall6(nr, atFDCWD, uintptr(unsafe.Pointer(oldPtr)), atFDCWD, uintptr(unsafe.Pointer(newPtr)), renameNoReplace, 0)
	if errno != 0 {
		if errors.Is(errno, syscall.EEXIST) {
			return fmt.Errorf("destination exists: %w", syscall.EEXIST)
		}
		return errno
	}
	oldParent := filepath.Dir(oldPath)
	newParent := filepath.Dir(newPath)
	if err := SyncDirectory(oldParent); err != nil {
		return err
	}
	if filepath.Clean(newParent) != filepath.Clean(oldParent) {
		if err := SyncDirectory(newParent); err != nil {
			return err
		}
	}
	return nil
}
