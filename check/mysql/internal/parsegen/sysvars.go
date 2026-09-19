package parsegen

// The system variable scope table: which names `@@session.x` / `@@global.x` may read
// (sql/sys_vars.cc's declarations -- the server refuses the wrong scope with 1238,
// "Variable 'x' is a GLOBAL variable" / "... is a SESSION variable"). Each `static
// Sys_var_* name(...)` declaration carries its scope as the SESSION_VAR / GLOBAL_VAR /
// SESSION_ONLY macro (sys_vars.h) or the raw sys_var::ONLY_SESSION / sys_var::GLOBAL
// constant; the handful of classes that fix the scope in their own constructor
// (Sys_var_gtid_executed and friends, sys_vars.h) are listed here by class. A
// Sys_var_deprecated_alias("old", Sys_new) takes the aliased declaration's scope.
//
// Declarations under a preprocessor condition that a release Linux server does not
// compile (#ifndef NDEBUG's debug, ENABLED_DEBUG_SYNC's debug_sync, _WIN32's named
// pipes) are left out: the generated table only says what every stock server has, and
// a name outside the table is not judged (a plugin's or component's variable, say).
// TestSysVarScopeServer pins every generated entry against mysqld.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// SysVarScope is one variable's scope: 's' session-only, 'g' global-only, 'b' both.
type SysVarScope byte

// sysVarClassScope fixes the scope of the classes whose constructor sets it without a
// scope macro in the declaration (sys_vars.h's own class definitions).
var sysVarClassScope = map[string]SysVarScope{
	"Sys_var_gtid_executed":    'g', // Sys_var_gtid_executed's ctor: GLOBAL
	"Sys_var_gtid_owned":       'b', // Sys_var_gtid_owned's ctor: SESSION (both scopes)
	"Sys_var_proxy_user":       's', // Sys_var_proxy_user's ctor: ONLY_SESSION
	"Sys_var_external_user":    's', // Sys_var_external_user extends Sys_var_proxy_user
	"Sys_var_test_flag":        'g', // Sys_var_test_flag's ctor: READ_ONLY GLOBAL_VAR
	"Sys_var_system_time_zone": 'g', // Sys_var_system_time_zone's ctor: GLOBAL
}

// sysVarDefined is the preprocessor state of a stock release Linux build: the symbols a
// release server defines (NDEBUG among them) and, by absence, the ones it does not
// (_WIN32, WITH_LOCK_ORDER, ENABLED_DEBUG_SYNC, WITH_HYPERGRAPH_OPTIMIZER, HAVE_UBSAN).
var sysVarDefined = map[string]bool{
	"NDEBUG":                         true,
	"HAVE_SYS_TIME_H":                true,
	"HAVE_UNISTD_H":                  true,
	"HAVE_MLOCKALL":                  true,
	"WITH_PERFSCHEMA_STORAGE_ENGINE": true,
	"MYSQL_ICU_DATADIR":              true,
	"HAVE_BUILD_ID_SUPPORT":          true,
}

var (
	sysVarDecl   = regexp.MustCompile(`(?m)^static\s+(Sys_var_\w+)(?:<[^>]*>)?\s+(\w+)\s*\(`)
	sysVarString = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)
	sysVarPP     = regexp.MustCompile(`^\s*#\s*(if|ifdef|ifndef|elif|else|endif)\b(.*)$`)
)

// ReadSysVars reads the server's system variable declarations into name -> scope.
func ReadSysVars(src string) (map[string]SysVarScope, error) {
	out := map[string]SysVarScope{}
	type alias struct {
		name string // the alias's own variable name
		base string // the Sys_* identifier it forwards to
	}
	var aliases []alias
	var unclassified []string
	byIdent := map[string]string{} // Sys_* identifier -> variable name
	// a handful of declarations name the variable through a macro (set_var.h's
	// PERSISTED_GLOBALS_LOAD and friends) rather than a string literal
	nameMacros := map[string]string{}
	if setVar, err := os.ReadFile(filepath.Join(src, "sql", "set_var.h")); err == nil {
		for _, m := range regexp.MustCompile(`(?m)^#define\s+(\w+)\s+"([a-z0-9_]+)"`).FindAllStringSubmatch(string(setVar), -1) {
			nameMacros[m[1]] = m[2]
		}
	}
	for _, file := range []string{"sys_vars.cc", "ssl_init_callback.cc"} {
		text, err := os.ReadFile(filepath.Join(src, "sql", file))
		if err != nil {
			return nil, err
		}
		lines := strings.Split(string(text), "\n")
		// live[i] reports whether line i is compiled under sysVarDefined.
		live := ppLive(lines)
		body := string(text)
		offsets := lineOffsets(body)
		for _, m := range sysVarDecl.FindAllStringSubmatchIndex(body, -1) {
			class := body[m[2]:m[3]]
			line := sort.Search(len(offsets), func(i int) bool { return offsets[i] > m[0] }) - 1
			if !live[line] {
				continue
			}
			decl := declText(body, m[1]-1) // from the opening parenthesis
			if strings.Contains(decl, "NOT_VISIBLE") {
				continue // a command-line-only setting: @@ cannot read it (1193)
			}
			// the name is the first argument: a string literal, or a macro
			// (nameMacros) that expands to one
			first := strings.TrimLeft(decl[1:], " \n\t")
			var name string
			if strings.HasPrefix(first, `"`) {
				names := sysVarString.FindAllStringSubmatch(decl, 1)
				if len(names) == 0 {
					continue
				}
				name = strings.ToLower(names[0][1])
			} else if mac := regexp.MustCompile(`^\w+`).FindString(first); nameMacros[mac] != "" {
				name = nameMacros[mac]
			} else {
				unclassified = append(unclassified, fmt.Sprintf("%s %s: unreadable name argument %.30q", class, body[m[4]:m[5]], first))
				continue
			}
			ident := body[m[4]:m[5]]
			byIdent[ident] = name
			if class == "Sys_var_deprecated_alias" {
				// ("old_name", Sys_base): the base identifier is the second argument
				parts := strings.SplitN(decl, ",", 3)
				if len(parts) >= 2 {
					aliases = append(aliases, alias{name: name, base: strings.TrimFunc(parts[1], func(r rune) bool {
						return r == ' ' || r == '\n' || r == ')' || r == ';' || r == '\t'
					})})
				}
				continue
			}
			scope, ok := declScope(class, decl)
			if !ok {
				unclassified = append(unclassified, fmt.Sprintf("%s %s (%s)", class, ident, name))
				continue
			}
			out[name] = scope
		}
	}
	if len(unclassified) > 0 {
		return nil, fmt.Errorf("sysvars: no scope marker and no class default:\n  %s", strings.Join(unclassified, "\n  "))
	}
	for _, al := range aliases {
		base, ok := byIdent[al.base]
		if !ok {
			return nil, fmt.Errorf("sysvars: alias %s forwards to unknown identifier %s", al.name, al.base)
		}
		scope, ok := out[base]
		if !ok {
			return nil, fmt.Errorf("sysvars: alias %s forwards to %s, which has no scope", al.name, base)
		}
		out[al.name] = scope
	}
	return out, nil
}

// declScope reads one declaration's scope marker.
func declScope(class, decl string) (SysVarScope, bool) {
	switch {
	case strings.Contains(decl, "SESSION_ONLY(") || strings.Contains(decl, "sys_var::ONLY_SESSION"):
		return 's', true
	case strings.Contains(decl, "SESSION_VAR("):
		return 'b', true
	case strings.Contains(decl, "GLOBAL_VAR(") || strings.Contains(decl, "KEYCACHE_VAR(") || regexp.MustCompile(`sys_var::GLOBAL\b`).MatchString(decl):
		return 'g', true
	}
	s, ok := sysVarClassScope[class]
	return s, ok
}

// declText returns the declaration's text from its opening parenthesis to the matching
// close, skipping string literals (a ')' inside one does not count).
func declText(body string, open int) string {
	depth := 0
	for i := open; i < len(body); i++ {
		switch body[i] {
		case '"':
			for i++; i < len(body) && body[i] != '"'; i++ {
				if body[i] == '\\' {
					i++
				}
			}
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return body[open : i+1]
			}
		}
	}
	return body[open:]
}

// lineOffsets is each line's byte offset.
func lineOffsets(body string) []int {
	offs := []int{0}
	for i := 0; i < len(body); i++ {
		if body[i] == '\n' {
			offs = append(offs, i+1)
		}
	}
	return offs
}

// ppLive walks the preprocessor conditionals and reports, per line, whether a release
// Linux build compiles it. A condition it cannot read (an arithmetic #if) is taken as
// live on both branches' declarations being rare enough for the server pin to catch.
func ppLive(lines []string) []bool {
	type frame struct {
		live  bool // this branch is compiled (under the enclosing frames)
		taken bool // some branch of this #if was already taken
		known bool // the condition was readable; unknown conditions keep both branches live
	}
	live := make([]bool, len(lines))
	var stack []frame
	cur := func() bool {
		for _, f := range stack {
			if !f.live {
				return false
			}
		}
		return true
	}
	for i, ln := range lines {
		m := sysVarPP.FindStringSubmatch(ln)
		if m == nil {
			live[i] = cur()
			continue
		}
		live[i] = false // the directive line itself holds no declaration
		switch m[1] {
		case "if", "ifdef", "ifndef":
			v, known := ppEval(m[1], m[2])
			stack = append(stack, frame{live: !known || v, taken: known && v, known: known})
		case "elif":
			if len(stack) > 0 {
				f := &stack[len(stack)-1]
				v, known := ppEval("if", m[2])
				if !f.known || !known {
					f.live, f.known = true, false
					break
				}
				f.live = !f.taken && v
				f.taken = f.taken || v
			}
		case "else":
			if len(stack) > 0 {
				f := &stack[len(stack)-1]
				if f.known {
					f.live = !f.taken
				}
			}
		case "endif":
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	return live
}

// ppEval reads an #if / #ifdef / #ifndef condition against sysVarDefined; ok is false
// for an expression it does not understand.
func ppEval(kind, cond string) (value, ok bool) {
	cond = strings.TrimSpace(strings.SplitN(cond, "//", 2)[0])
	if i := strings.Index(cond, "/*"); i >= 0 {
		cond = strings.TrimSpace(cond[:i])
	}
	switch kind {
	case "ifdef":
		return sysVarDefined[cond], true
	case "ifndef":
		return !sysVarDefined[cond], true
	}
	if m := regexp.MustCompile(`^!?\s*defined\s*\(\s*(\w+)\s*\)$`).FindStringSubmatch(cond); m != nil {
		v := sysVarDefined[m[1]]
		if strings.HasPrefix(cond, "!") {
			v = !v
		}
		return v, true
	}
	if m := regexp.MustCompile(`^defined\s*\(\s*(\w+)\s*\)\s*&&\s*defined\s*\(\s*(\w+)\s*\)$`).FindStringSubmatch(cond); m != nil {
		return sysVarDefined[m[1]] && sysVarDefined[m[2]], true
	}
	return false, false
}

// SysVarsGo renders the generated catalog table.
func SysVarsGo(pkg, version string, vars map[string]SysVarScope) string {
	names := make([]string, 0, len(vars))
	for n := range vars {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	fmt.Fprintf(&b, "// Code generated from MySQL %s (sql/sys_vars.cc) by parsegen. DO NOT EDIT.\n\npackage %s\n\n", version, pkg)
	b.WriteString("// SysVars is each system variable's scope: 's' session-only, 'g' global-only,\n// 'b' both. A name outside the map (a plugin's or component's variable) is not judged.\nvar SysVars = map[string]byte{\n")
	for _, n := range names {
		fmt.Fprintf(&b, "\t%q: '%c',\n", n, vars[n])
	}
	b.WriteString("}\n")
	return b.String()
}
