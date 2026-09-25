//go:build linux && amd64

package filesystem

func renameat2SyscallNumber() (uintptr, bool) { return 316, true }
