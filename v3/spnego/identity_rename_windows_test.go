//go:build windows

package spnego

// identityDetectsRename is false on Windows: fileRevisionID there offers only
// the creation time, which NTFS file tunneling can restore onto a replacement
// made at the same name. Flip this if identity_windows.go ever adopts a
// dependable identity.
const identityDetectsRename = false
