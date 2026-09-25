//go:build linux && !amd64 && !arm64

package filesystem

func renameat2SyscallNumber() (uintptr, bool) { return 0, false }
