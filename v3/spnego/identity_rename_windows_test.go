//go:build windows

package spnego

// False on Windows: fileRevisionID offers only the creation time, which NTFS
// tunneling can restore onto a same-named replacement. Flip if that changes.
const identityDetectsRename = false
