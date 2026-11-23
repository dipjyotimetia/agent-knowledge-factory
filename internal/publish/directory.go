package publish

import (
	"context"
	"os"
	"path"
)

// Directory writes a knowledge tree to a local directory. It is a second
// storage format. GitHub remains the default publisher and the only one that opens a pull request.
type Directory struct {
	Root string
}

// Write replaces files under Root. It does not merge and it does not delete unrelated files.
func (d Directory) Write(_ context.Context, files map[string]string) error {
	root, err := os.OpenRoot(d.Root)
	if err != nil {
		return err
	}
	defer root.Close()
	for rel, body := range files {
		rel = path.Clean(rel)
		if rel == "." || rel == "" || path.IsAbs(rel) || len(rel) >= 2 && rel[0] == '.' && rel[1] == '.' {
			return os.ErrInvalid
		}
		if dir := path.Dir(rel); dir != "." {
			if err := root.MkdirAll(dir, 0o750); err != nil {
				return err
			}
		}
		if err := root.WriteFile(rel, []byte(body), 0o600); err != nil {
			return err
		}
	}
	return nil
}
