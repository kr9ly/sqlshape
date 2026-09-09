package schema

import (
	"strconv"
	"strings"

	"github.com/kr9ly/sqlshape/mysql/internal/mysqlast"
)

// Type is a MySQL column type as declared.
type Type struct {
	// Name is the MySQL type name in lower case: int, bigint, decimal, varchar, text,
	// datetime, json, enum, set, point ...
	Name      string
	Length    int // display width / length / precision; -1 when not given
	Dec       int // scale / fractional seconds; -1 when not given
	Unsigned  bool
	Zerofill  bool
	Binary    bool   // CHAR(n) BINARY, i.e. the binary collation
	Charset   string // CHARACTER SET on a string type
	Collation string
	Values    []string // ENUM / SET members
	Srid      int      // SRID on a spatial type; 0 when none
}

// String renders the type the way SHOW CREATE TABLE would, modulo MySQL's own rewrites.
func (t Type) String() string {
	var b strings.Builder
	b.WriteString(t.Name)
	switch {
	case len(t.Values) > 0:
		b.WriteByte('(')
		for i, v := range t.Values {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString("'" + strings.ReplaceAll(v, "'", "''") + "'")
		}
		b.WriteByte(')')
	case t.Length >= 0 && t.Dec >= 0:
		b.WriteString("(" + strconv.Itoa(t.Length) + "," + strconv.Itoa(t.Dec) + ")")
	case t.Length >= 0:
		b.WriteString("(" + strconv.Itoa(t.Length) + ")")
	case t.Dec >= 0:
		b.WriteString("(" + strconv.Itoa(t.Dec) + ")")
	}
	if t.Unsigned {
		b.WriteString(" unsigned")
	}
	if t.Zerofill {
		b.WriteString(" zerofill")
	}
	if t.Charset != "" {
		b.WriteString(" CHARACTER SET " + t.Charset)
	}
	if t.Binary {
		b.WriteString(" BINARY")
	}
	if t.Collation != "" {
		b.WriteString(" COLLATE " + t.Collation)
	}
	if t.Srid != 0 {
		b.WriteString(" SRID " + strconv.Itoa(t.Srid))
	}
	return b.String()
}

// IsString reports a character string type (charset and collation apply).
func (t Type) IsString() bool {
	switch t.Name {
	case "char", "varchar", "tinytext", "text", "mediumtext", "longtext", "enum", "set":
		return true
	}
	return false
}

// IsInteger reports an integer type.
func (t Type) IsInteger() bool {
	switch t.Name {
	case "tinyint", "smallint", "mediumint", "int", "bigint":
		return true
	}
	return false
}

// typeOf reads a PT_*_type node.
func (s *Schema) typeOf(v mysqlast.Value, at func(mysqlast.Value) int) Type {
	t := Type{Length: -1, Dec: -1}
	n, ok := v.(*mysqlast.Node)
	if !ok {
		s.problem(at(v), "type not understood: %s", mysqlast.Sprint(v))
		return t
	}
	switch n.Class {
	case "PT_numeric_type":
		t.Name = enumName(str(n.Arg("type")))
		t.Length = intOr(n.Arg("length"), -1)
		t.Dec = intOr(n.Arg("dec"), -1)
		opts := str(n.Arg("options"))
		t.Unsigned = strings.Contains(opts, "UNSIGNED_FLAG")
		t.Zerofill = strings.Contains(opts, "ZEROFILL_FLAG")
		if t.Zerofill {
			t.Unsigned = true // ZEROFILL implies UNSIGNED
		}
	case "PT_char_type":
		t.Name = enumName(str(n.Arg("char_type")))
		t.Length = intOr(n.Arg("length"), -1)
		if t.Name == "char" && t.Length < 0 {
			t.Length = 1
		}
		t.Charset = charsetName(n.Arg("charset"))
		t.Binary = isTrue(n.Arg("force_binary"))
	case "PT_blob_type":
		t.Name = blobName(str(n.Arg("blob_type")), n.Arg("charset") != nil)
		t.Length = intOr(n.Arg("length"), -1)
		t.Charset = charsetName(n.Arg("charset"))
		t.Binary = isTrue(n.Arg("force_binary"))
	case "PT_time_type":
		t.Name = enumName(str(n.Arg("time_type")))
		t.Dec = intOr(n.Arg("dec"), -1)
	case "PT_timestamp_type":
		t.Name = "timestamp"
		t.Dec = intOr(n.Arg("dec"), -1)
	case "PT_date_type":
		t.Name = "date"
	case "PT_year_type":
		t.Name = "year"
	case "PT_bit_type":
		t.Name = "bit"
		t.Length = intOr(n.Arg("length"), -1)
	case "PT_boolean_type":
		t.Name = "tinyint"
		t.Length = 1
	case "PT_json_type":
		t.Name = "json"
	case "PT_serial_type":
		t.Name = "bigint"
		t.Unsigned = true
	case "PT_enum_type", "PT_set_type":
		t.Name = "enum"
		if n.Class == "PT_set_type" {
			t.Name = "set"
		}
		for _, m := range list(n.Arg("interval_list")) {
			if mn, ok := m.(*mysqlast.Node); ok && mn.Class == "String" {
				t.Values = append(t.Values, str(mn.Arg("str")))
			} else {
				t.Values = append(t.Values, str(m))
			}
		}
		t.Charset = charsetName(n.Arg("charset"))
		t.Binary = isTrue(n.Arg("force_binary"))
	case "PT_spacial_type":
		t.Name = strings.ToLower(strings.TrimPrefix(str(n.Arg("geo_type")), "Field::GEOM_"))
	default:
		s.problem(at(n), "type %s not understood", n.Class)
	}
	return t
}

// enumName lowers a server enum member (Int_type::INT, Char_type::VARCHAR, Numeric_type::DECIMAL, Time_type::DATETIME).
func enumName(s string) string {
	if i := strings.LastIndex(s, "::"); i >= 0 {
		s = s[i+2:]
	}
	return strings.ToLower(s)
}

// blobName maps Blob_type and the presence of a charset to the MySQL type name: TEXT
// variants carry a charset, BLOB variants do not. The server's PT_blob_type uses the same
// node for both and tells them apart by the charset.
func blobName(blobType string, text bool) string {
	prefix := ""
	switch enumName(blobType) {
	case "tiny":
		prefix = "tiny"
	case "medium":
		prefix = "medium"
	case "long":
		prefix = "long"
	}
	if text {
		return prefix + "text"
	}
	return prefix + "blob"
}

// charsetName renders a charset argument: the name, or "" for none. The server passes
// &my_charset_bin for BINARY / VARBINARY / BLOB and national_charset_info for NCHAR.
func charsetName(v mysqlast.Value) string {
	s := str(v)
	switch s {
	case "&my_charset_bin":
		return "binary"
	case "national_charset_info":
		return "utf8mb3"
	case "national_charset_info.bin":
		return "utf8mb3"
	}
	return s
}

func intOr(v mysqlast.Value, def int) int {
	switch x := v.(type) {
	case mysqlast.Number:
		return int(x)
	case string:
		if n, err := strconv.Atoi(x); err == nil {
			return n
		}
	case mysqlast.Token:
		if n, err := strconv.Atoi(str(x)); err == nil {
			return n
		}
	}
	return def
}
