package analyze

import "strings"

// aclitemError is the syntax side of aclparse (acl.c): "" when the text is an aclitem,
// else PG's message. Role names are not checked (the checker has no roles: PG's 42704
// there is a known lenient spot). Two readings of a quoted name: 17 read `""` as an
// escaped quote even when it opened the name, 18 opens and closes the name, so `/""` is
// an empty grantor there.
func aclitemError(s string, pg18 bool) string {
	name, rest := aclGetID(s, pg18)
	if !strings.HasPrefix(rest, "=") {
		if name != "group" && name != "user" {
			return "unrecognized key word: \"" + name + "\""
		}
		name, rest = aclGetID(rest, pg18)
		if name == "" {
			return "missing name"
		}
	}
	if !strings.HasPrefix(rest, "=") {
		return "missing \"=\" sign"
	}
	rest = rest[1:]
	const modes = "arwdDxtXUCTcsAm"
	for len(rest) > 0 && (isASCIILetter(rest[0]) || rest[0] == '*') {
		if rest[0] != '*' && !strings.ContainsRune(modes, rune(rest[0])) {
			return "invalid mode character: must be one of \"" + modes + "\""
		}
		rest = rest[1:]
	}
	if strings.HasPrefix(rest, "/") {
		grantor, after := aclGetID(rest[1:], pg18)
		if grantor == "" {
			return "a name must follow the \"/\" sign"
		}
		rest = after
	}
	if strings.TrimSpace(rest) != "" {
		return "extra garbage at the end of the ACL specification"
	}
	return ""
}

// aclGetID is getid: a name of identifier characters, or double-quoted with "" for a
// quote inside; surrounding blanks are skipped. It returns the name and what follows.
func aclGetID(s string, pg18 bool) (string, string) {
	s = strings.TrimLeft(s, " \t\n\r")
	var b strings.Builder
	inQuotes := false
	i := 0
	for ; i < len(s); i++ {
		c := s[i]
		if !(inQuotes || c == '"' || isASCIILetter(c) || c >= '0' && c <= '9' || c == '_' || c >= 0x80) {
			break
		}
		if c == '"' {
			if pg18 && !inQuotes {
				inQuotes = true
				continue
			}
			if i+1 < len(s) && s[i+1] == '"' {
				b.WriteByte('"') // an escaped quote
				i++
				continue
			}
			inQuotes = !inQuotes
			continue
		}
		b.WriteByte(c)
	}
	return b.String(), strings.TrimLeft(s[i:], " \t\n\r")
}

func isASCIILetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
