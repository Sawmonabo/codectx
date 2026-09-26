//go:build !unix

package sqlite

// probeDirAccess reports the directory's writability as unknown on a platform
// this build has no portable probe for. Unknown makes no claim about the file:
// the store is opened the ordinary read-only way, and a refusal the engine
// raises reaches the caller as the engine raised it.
func probeDirAccess(dir string) (dirAccess, error) { return dirAccessUnknown, nil }
