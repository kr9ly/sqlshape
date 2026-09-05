package schema

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ReadSource reads the schema at path: a .sql file, or a directory whose *.sql files are
// concatenated in name order (so `schema/010_types.sql`, `schema/020_tables.sql`, ...
// apply like one file). Each file's text is preceded by a comment naming it, so a
// problem's position can be traced back.
func ReadSource(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		b, err := os.ReadFile(path)
		return string(b), err
	}
	files, err := filepath.Glob(filepath.Join(path, "*.sql"))
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", fmt.Errorf("%s: no *.sql files", path)
	}
	sort.Strings(files)
	var b strings.Builder
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "-- file: %s\n", filepath.Base(f))
		b.Write(src)
		if len(src) > 0 && src[len(src)-1] != '\n' {
			b.WriteByte('\n')
		}
	}
	return b.String(), nil
}
