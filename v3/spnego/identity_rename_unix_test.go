//go:build unix

package spnego

// identityDetectsRename reports whether this platform's fileRevisionID reliably
// distinguishes a file replaced by rename from the file it replaced. Unix
// identity is the inode, which a rename always changes.
const identityDetectsRename = true
