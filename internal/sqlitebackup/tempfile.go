package sqlitebackup

import (
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// createTempFile creates a uniquely named file in dir. Unlike os.CreateTemp,
// which always forces mode 0600, it requests 0666 so the process umask and the
// directory's default ACL decide the real permissions. pattern must contain
// one "*", replaced with random characters.
func createTempFile(dir, pattern string) (*os.File, error) {
	prefix, suffix, ok := strings.Cut(pattern, "*")
	if !ok {
		return nil, fmt.Errorf("create temp file: pattern %q has no *", pattern)
	}
	for attempt := 0; attempt < 10000; attempt++ {
		var random [8]byte
		if _, err := crand.Read(random[:]); err != nil {
			return nil, fmt.Errorf("create temp file: random name: %w", err)
		}
		name := filepath.Join(dir, prefix+hex.EncodeToString(random[:])+suffix)
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("create temp file in %q: too many name collisions", dir)
}
