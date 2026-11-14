//go:build !unix

package sqlitebackup

import "os"

func linkCount(os.FileInfo) (uint64, bool) { return 0, false }
