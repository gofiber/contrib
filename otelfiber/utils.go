package otelfiber

// This is a minimal port from `github.com/gofiber/fiber/v2/utils` to unbundle from v2

import "unsafe"

// UnsafeString returns a string pointer without allocation
func UnsafeString(b []byte) string {
	return unsafe.String(unsafe.SliceData(b), len(b))
}

// UnsafeBytes returns a byte pointer without allocation.
func UnsafeBytes(s string) []byte {
	return unsafe.Slice(unsafe.StringData(s), len(s))
}

// CopyString copies a string to make it immutable
func CopyString(s string) string {
	return string(UnsafeBytes(s))
}

// CopyBytes copies a slice to make it immutable
func CopyBytes(b []byte) []byte {
	tmp := make([]byte, len(b))
	copy(tmp, b)
	return tmp
}
