package collection

import "slices"

func AppendUnique[T comparable](values []T, value T) []T {
	if slices.Contains(values, value) {
		return values
	}
	return append(values, value)
}
