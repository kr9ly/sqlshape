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
// ReadSource reads a schema.sql file, or a directory of *.sql files in name order, and
// requires the text to declare its PostgreSQL version (`-- sqlshape: postgres 17`): the
// version decides the grammar, the catalog and the PostgreSQL pgtest runs, so a file
// without one is not judged. Schema text built in code (Load) may leave it out and gets
// pgparse.Default.
func ReadSource(path string) (string, error) {
	text, err := ReadText(path)
	if err != nil {
		return "", err
	}
	if err := RequireVersion(path, text); err != nil {
		return "", err
	}
	return text, nil
}

// RequireVersion is ReadSource's check on a text already read: the PostgreSQL version
// must be declared.
func RequireVersion(path, text string) error {
	if !versionLine.MatchString(strings.TrimPrefix(text, "\ufeff")) { // a byte order mark is not part of the SQL
		return fmt.Errorf("%s: declare the PostgreSQL version the schema is written for, as a line `-- sqlshape: postgres 17` (sqlshape supports PostgreSQL %s)", path, supportedVersions())
	}
	return nil
}

// ReadText is ReadSource without the version check: the schema text of a file or a
// directory, for a caller that reads the dialect line itself (internal/dialect).
func ReadText(path string) (string, error) {
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
