//go:build !unix && !windows

package spnego

// identityDetectsRename is false: fileRevisionID exposes no identity on these
// platforms, so change detection uses size and modification time alone.
const identityDetectsRename = false
