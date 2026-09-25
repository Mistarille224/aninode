//go:build linux

package filesystem

import (
	"errors"
	"fmt"
	"path/filepath"
	"syscall"
	"unsafe"
)

const atRemovedir = 0x200

// RemoveObservedLink removes exactly the observed regular-file directory entry
// beneath root. Directory traversal is anchored with O_NOFOLLOW and the inode
// is verified immediately before unlinkat, so a stale observation cannot cause
// removal of a replacement file.
func RemoveObservedLink(root, path string, expected ObjectID) error {
	rel, err := relativeBelow(root, path)
	if err != nil {
		return err
	}
	rootFD, err := openDirectoryNoFollow(root)
	if err != nil {
		return fmt.Errorf("open removal root: %w", err)
	}
	defer syscall.Close(rootFD)
	parentFD, err := openRelativeDirectory(rootFD, filepath.Dir(rel), false, 0)
	if err != nil {
		return fmt.Errorf("open removal parent: %w", err)
	}
	defer syscall.Close(parentFD)
	name := filepath.Base(rel)
	fd, err := syscall.Openat(parentFD, name, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, syscall.ENOENT) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open removal candidate: %w", err)
	}
	if err := verifyFD(fd, expected); err != nil {
		syscall.Close(fd)
		return fmt.Errorf("refuse to remove changed publication %s: %w", path, err)
	}
	if err := syscall.Close(fd); err != nil {
		return err
	}
	if err := rawUnlinkat(parentFD, name); err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return nil
		}
		return err
	}
	return syscall.Fsync(parentFD)
}

// RemoveEmptyParents prunes empty directories from start upward, stopping
// before root. It never follows symlinks and treats non-empty/missing parents
// as a normal stopping condition.
func RemoveEmptyParents(root, start string) error {
	root = filepath.Clean(root)
	current := filepath.Clean(start)
	for current != root {
		rel, err := relativeBelow(root, current)
		if err != nil {
			return err
		}
		parentRel := filepath.Dir(rel)
		name := filepath.Base(rel)
		rootFD, err := openDirectoryNoFollow(root)
		if err != nil {
			return err
		}
		parentFD, err := openRelativeDirectory(rootFD, parentRel, false, 0)
		if err != nil {
			syscall.Close(rootFD)
			if errors.Is(err, syscall.ENOENT) {
				return nil
			}
			return err
		}
		err = rawUnlinkatFlags(parentFD, name, atRemovedir)
		if err == nil {
			_ = syscall.Fsync(parentFD)
		}
		syscall.Close(parentFD)
		syscall.Close(rootFD)
		if errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) || errors.Is(err, syscall.ENOENT) {
			return nil
		}
		if err != nil {
			return err
		}
		current = filepath.Dir(current)
	}
	return nil
}

func rawUnlinkatFlags(dirFD int, name string, flags int) error {
	ptr, err := syscall.BytePtrFromString(name)
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall(syscall.SYS_UNLINKAT, uintptr(dirFD), uintptr(unsafe.Pointer(ptr)), uintptr(flags))
	if errno != 0 {
		return errno
	}
	return nil
}
