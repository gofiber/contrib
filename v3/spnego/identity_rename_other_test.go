//go:build !unix

package spnego

// identityDetectsRename is false off Unix. Windows offers only the creation
// time, which NTFS file tunneling can restore onto a replacement made at the
// same name, and other platforms expose no identity at all. See the
// fileRevisionID implementations.
const identityDetectsRename = false
