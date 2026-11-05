// Package runfolder resolves folders on the receiving machine.
package runfolder

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Resolve cleans a path and resolves its longest existing prefix. Missing
// suffixes stay attached to that prefix; a dangling symlink is an error.
func Resolve(path, home string) (string, error) {
	if path == "" || strings.ContainsRune(path, 0) {
		return "", fmt.Errorf("invalid run folder %q", path)
	}
	if path == "~" {
		path = home
	} else if strings.HasPrefix(path, "~/") {
		path = filepath.Join(home, path[2:])
	} else if !filepath.IsAbs(path) {
		path = filepath.Join(home, path)
	}
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("receiver home must be absolute")
	}
	prefix := path
	var suffix []string
	for {
		info, err := os.Lstat(prefix)
		if err == nil {
			resolved, err := filepath.EvalSymlinks(prefix)
			if err != nil {
				return "", err
			}
			if !info.IsDir() {
				info, err = os.Stat(resolved)
				if err != nil || !info.IsDir() {
					return "", fmt.Errorf("run folder prefix %q is not a directory", prefix)
				}
			}
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return resolved, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		suffix = append(suffix, filepath.Base(prefix))
		prefix = filepath.Dir(prefix)
	}
}

// Contains compares whole path components, so /code-other is not in /code.
func Contains(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
