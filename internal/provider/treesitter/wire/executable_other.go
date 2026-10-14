//go:build !unix

package wire

import "os"

// fileNumber is empty where a file's stat carries no file number: size and
// modification time identify it.
func fileNumber(os.FileInfo) string { return "" }
