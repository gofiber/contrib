//go:build !unix && !windows

package spnego

// False: no identity is exposed, so detection uses size and modification time alone.
const identityDetectsRename = false
