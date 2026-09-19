package dialect

import (
	"fmt"
	"regexp"
	"strings"
)

// Setting is one `-- sqlshape: server <name> = <value>` line: a server variable the
// schema declares the database runs with, because the variable changes how statements
// are judged (MySQL's sql_mode, lower_case_table_names). The syntax is the same for every
// dialect; which names a dialect accepts, and what each does to its judgments, is the
// dialect's (an unknown name is a problem of the schema).
type Setting struct {
	Name     string // lower-cased, as server variables are case-insensitive
	Value    string // the literal's content ('...' unquoted, '' unescaped) or the bare token
	Position int    // byte offset of the line (its first character) in the schema text
}

var settingLine = regexp.MustCompile(`(?m)^[ \t]*--[ \t]*sqlshape:[ \t]*server\b[ \t]*(.*?)[ \t]*$`)

// Settings reads the schema's server settings, in order. A malformed line, or a variable
// declared twice, is an error.
func Settings(schemaSQL string) ([]Setting, error) {
	var out []Setting
	seen := map[string]bool{}
	for _, m := range settingLine.FindAllStringSubmatchIndex(schemaSQL, -1) {
		text := schemaSQL[m[2]:m[3]]
		s, err := parseSetting(text)
		if err != nil {
			return nil, fmt.Errorf("schema: `-- sqlshape: server %s`: %w", text, err)
		}
		if seen[s.Name] {
			return nil, fmt.Errorf("schema: `-- sqlshape: server %s`: %s is declared twice", text, s.Name)
		}
		seen[s.Name] = true
		s.Position = m[0]
		out = append(out, s)
	}
	return out, nil
}

// IsSetting reports whether a directive's text (what follows `-- sqlshape:`) is a server
// setting: the loaders leave these out of a statement's directives.
func IsSetting(directive string) bool {
	f := strings.Fields(directive)
	return len(f) > 0 && strings.EqualFold(f[0], "server")
}

var settingName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)

func parseSetting(text string) (Setting, error) {
	name, value, ok := strings.Cut(text, "=")
	if !ok {
		return Setting{}, fmt.Errorf("want `<variable> = <value>`")
	}
	name, value = strings.TrimSpace(name), strings.TrimSpace(value)
	if !settingName.MatchString(name) {
		return Setting{}, fmt.Errorf("%q is not a variable name", name)
	}
	if value == "" {
		return Setting{}, fmt.Errorf("want a value after `=`")
	}
	if value[0] == '\'' {
		if len(value) < 2 || value[len(value)-1] != '\'' {
			return Setting{}, fmt.Errorf("the string literal is not closed")
		}
		body := value[1 : len(value)-1]
		if strings.Contains(strings.ReplaceAll(body, "''", ""), "'") {
			return Setting{}, fmt.Errorf("a quote inside the string literal is written ''")
		}
		value = strings.ReplaceAll(body, "''", "'")
	} else if strings.ContainsAny(value, " \t") {
		return Setting{}, fmt.Errorf("a value with spaces is written as a string literal '...'")
	}
	return Setting{Name: strings.ToLower(name), Value: value}, nil
}
