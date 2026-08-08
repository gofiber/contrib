//go:build unix

package spnego

// identityDetectsRename reports whether fileRevisionID distinguishes a replaced file
// from its replacement. Unix identity is the inode, which a rename always changes.
const identityDetectsRename = true
