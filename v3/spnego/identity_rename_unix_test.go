//go:build unix

package spnego

// Whether fileRevisionID spots a rename. Unix identity is the inode, which always moves.
const identityDetectsRename = true
