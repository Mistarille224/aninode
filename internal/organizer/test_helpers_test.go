package organizer

import (
	"os"

	"aninode/internal/filesystem"
)

func sameFile(first, second string) bool {
	firstInfo, firstErr := os.Lstat(first)
	secondInfo, secondErr := os.Lstat(second)
	return firstErr == nil && secondErr == nil && firstInfo.Mode().IsRegular() && secondInfo.Mode().IsRegular() && filesystem.SameObject(first, second)
}
