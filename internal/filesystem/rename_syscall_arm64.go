//go:build linux && arm64

package filesystem

func renameat2SyscallNumber() (uintptr, bool) { return 276, true }
