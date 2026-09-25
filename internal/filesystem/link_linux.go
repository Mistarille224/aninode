//go:build linux

package filesystem

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// LinkObserved creates a hardlink between paths beneath fixed real directory
// roots. Every traversed directory is opened with O_NOFOLLOW, so a concurrent
// symlink cannot redirect traversal outside the observed namespace. The source
// ObjectID is checked both before and after linkat; destination creation is
// atomic and never replaces an existing entry.
func LinkObserved(sourceRoot, sourcePath, targetRoot, targetPath string, expected ObjectID, dirMode fs.FileMode) (bool, error) {
	sourceRel, err := relativeBelow(sourceRoot, sourcePath)
	if err != nil {
		return false, err
	}
	targetRel, err := relativeBelow(targetRoot, targetPath)
	if err != nil {
		return false, err
	}
	sourceRootFD, err := openDirectoryNoFollow(sourceRoot)
	if err != nil {
		return false, fmt.Errorf("open source root: %w", err)
	}
	defer syscall.Close(sourceRootFD)
	targetRootFD, err := openDirectoryNoFollow(targetRoot)
	if err != nil {
		return false, fmt.Errorf("open target root: %w", err)
	}
	defer syscall.Close(targetRootFD)

	sourceParentFD, err := openRelativeDirectory(sourceRootFD, filepath.Dir(sourceRel), false, dirMode)
	if err != nil {
		return false, fmt.Errorf("open source parent: %w", err)
	}
	defer syscall.Close(sourceParentFD)
	targetParentFD, err := openRelativeDirectory(targetRootFD, filepath.Dir(targetRel), true, dirMode)
	if err != nil {
		return false, fmt.Errorf("open target parent: %w", err)
	}
	defer syscall.Close(targetParentFD)

	sourceName, targetName := filepath.Base(sourceRel), filepath.Base(targetRel)
	sourceFD, err := syscall.Openat(sourceParentFD, sourceName, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return false, fmt.Errorf("open source object: %w", err)
	}
	defer syscall.Close(sourceFD)
	if err := verifyFD(sourceFD, expected); err != nil {
		return false, err
	}

	if err := rawLinkat(sourceParentFD, sourceName, targetParentFD, targetName); err != nil {
		if errors.Is(err, syscall.EEXIST) {
			targetFD, openErr := syscall.Openat(targetParentFD, targetName, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
			if openErr == nil {
				defer syscall.Close(targetFD)
				if verifyErr := verifyFD(targetFD, expected); verifyErr == nil {
					return false, nil
				}
			}
			return false, fs.ErrExist
		}
		return false, err
	}

	targetFD, err := syscall.Openat(targetParentFD, targetName, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		_ = rawUnlinkat(targetParentFD, targetName)
		return false, fmt.Errorf("verify new hardlink: %w", err)
	}
	verifyErr := verifyFD(targetFD, expected)
	_ = syscall.Close(targetFD)
	if verifyErr != nil {
		_ = rawUnlinkat(targetParentFD, targetName)
		return false, fmt.Errorf("verify new hardlink identity: %w", verifyErr)
	}
	if err := verifyFD(sourceFD, expected); err != nil {
		_ = rawUnlinkat(targetParentFD, targetName)
		_ = syscall.Fsync(targetParentFD)
		return false, fmt.Errorf("source changed during hardlink creation: %w", err)
	}
	if err := syscall.Fsync(targetParentFD); err != nil {
		return false, fmt.Errorf("sync hardlink parent: %w", err)
	}
	return true, nil
}

func relativeBelow(root, path string) (string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(rootAbs, pathAbs)
	if err != nil || rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %s is outside root %s", path, root)
	}
	return rel, nil
}

func openDirectoryNoFollow(path string) (int, error) {
	return syscall.Open(path, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
}

func openRelativeDirectory(rootFD int, rel string, create bool, mode fs.FileMode) (int, error) {
	current, err := syscall.Dup(rootFD)
	if err != nil {
		return -1, err
	}
	if rel == "." || rel == "" {
		return current, nil
	}
	for _, part := range strings.Split(filepath.Clean(rel), string(filepath.Separator)) {
		if part == "" || part == "." || part == ".." {
			syscall.Close(current)
			return -1, fmt.Errorf("unsafe directory component %q", part)
		}
		next, openErr := syscall.Openat(current, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
		if errors.Is(openErr, syscall.ENOENT) && create {
			created := false
			if mkdirErr := syscall.Mkdirat(current, part, uint32(mode.Perm())); mkdirErr != nil {
				if !errors.Is(mkdirErr, syscall.EEXIST) {
					syscall.Close(current)
					return -1, mkdirErr
				}
			} else {
				created = true
			}
			if created {
				if syncErr := syscall.Fsync(current); syncErr != nil {
					syscall.Close(current)
					return -1, syncErr
				}
			}
			next, openErr = syscall.Openat(current, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
		}
		if openErr != nil {
			syscall.Close(current)
			return -1, openErr
		}
		syscall.Close(current)
		current = next
	}
	return current, nil
}

func verifyFD(fd int, expected ObjectID) error {
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return errors.New("object is not a regular file")
	}
	got := ObjectID{Device: uint64(st.Dev), Inode: uint64(st.Ino)}
	if got != expected {
		return fmt.Errorf("filesystem object changed: got %+v want %+v", got, expected)
	}
	return nil
}

func rawLinkat(oldDirFD int, oldName string, newDirFD int, newName string) error {
	oldPtr, err := syscall.BytePtrFromString(oldName)
	if err != nil {
		return err
	}
	newPtr, err := syscall.BytePtrFromString(newName)
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall6(syscall.SYS_LINKAT, uintptr(oldDirFD), uintptr(unsafe.Pointer(oldPtr)), uintptr(newDirFD), uintptr(unsafe.Pointer(newPtr)), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

func rawUnlinkat(dirFD int, name string) error {
	ptr, err := syscall.BytePtrFromString(name)
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall(syscall.SYS_UNLINKAT, uintptr(dirFD), uintptr(unsafe.Pointer(ptr)), 0)
	if errno != 0 {
		return errno
	}
	return nil
}
