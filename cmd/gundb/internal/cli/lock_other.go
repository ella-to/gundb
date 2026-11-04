//go:build !unix

package cli

import "os"

// Without flock, run one group command at a time per user.
func lockFile(*os.File) error   { return nil }
func unlockFile(*os.File) error { return nil }
