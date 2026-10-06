package lockfile

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteAtomic writes data to path atomically: the payload goes to a private
// temporary file in the same directory, is fsynced, and then renamed over
// path. A failure at any step removes the temporary file and leaves the
// previous content of path (if any) untouched, so a failed run never leaves
// a half-written or truncated lock behind. Concurrent writers each use
// their own temporary file; the rename makes each completed write visible
// whole, never interleaved.
func WriteAtomic(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".golock-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp lock file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			os.Remove(tmpName)
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp lock file: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp lock file: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close temp lock file: %w", err)
	}
	if err = os.Chmod(tmpName, perm); err != nil {
		return fmt.Errorf("chmod temp lock file: %w", err)
	}
	if err = os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename lock into place: %w", err)
	}
	return nil
}
