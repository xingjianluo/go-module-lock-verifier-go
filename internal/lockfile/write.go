package lockfile

import (
	"os"
	"path/filepath"
)

// WriteAtomic writes data to path atomically: the bytes go to a temporary
// file in the same directory, are fsynced, and then renamed over path.
// A failure at any step removes the temporary file and leaves the previous
// content of path (if any) untouched, so a crashed or failed run never
// leaves a half-written lock behind.
func WriteAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".golock-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func(err error) error {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return cleanup(err)
	}
	if err := tmp.Sync(); err != nil {
		return cleanup(err)
	}
	if err := tmp.Close(); err != nil {
		return cleanup(err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return cleanup(err)
	}
	return nil
}
