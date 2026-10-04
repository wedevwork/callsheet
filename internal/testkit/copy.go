package testkit

import (
	"fmt"
	"os"
	"path/filepath"
)

// CopyTree copies the directory tree src to the new directory dst with
// each entry's exact permission bits (fixture setup: a prepared or warmed
// state reused without repeating its preparation). Only directories and
// regular files are expected; anything else fails.
func CopyTree(dst, src string) error {
	return filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		to := filepath.Join(dst, rel)
		switch {
		case fi.IsDir():
			if err := os.Mkdir(to, 0o700); err != nil {
				return err
			}
			return os.Chmod(to, fi.Mode().Perm())
		case fi.Mode().IsRegular():
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if err := os.WriteFile(to, b, 0o600); err != nil {
				return err
			}
			return os.Chmod(to, fi.Mode().Perm())
		}
		return fmt.Errorf("unexpected fixture entry %s (%v)", p, fi.Mode())
	})
}
