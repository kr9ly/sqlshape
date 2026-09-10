package parsegen

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The function catalog. MySQL has no pg_proc; what a function is called, how many
// arguments it takes and which Item class evaluates it is written in
// sql/item_create.cc (the native function registry), what that class returns is written
// in its base class and its resolve_type() (item*.h / item*.cc). This file reads those
// three sources.

// Func is one entry of the native function registry.
type Func struct {
	Name     string // upper case, as the registry spells it
	Class    string // the Item class the factory instantiates
	Min, Max int    // argument count range; Max = -1 for unbounded
	Odd      bool   // argument count must be odd
	Even     bool   // argument count must be even
	Internal bool   // only callable from system views
	// Factory is the special instantiator, when the registry used one (Round_instantiator);
	// its class is then what it finally constructs.
	Factory string
}

// ItemClass is what the headers say about an Item class.
type ItemClass struct {
	Name string
	Base string
	// Family is the nearest base that fixes the result kind: Item_int_func, Item_str_func,
	// Item_real_func, Item_dec_func, Item_func_numhybrid, Item_bool_func,
	// Item_datetime_func, Item_date_func, Item_time_func, Item_temporal_hybrid_func,
	// Item_json_func, Item_geometry_func, Item_sum ..., or "" when none applies.
	Family string
	// Facts are the declarative statements of the class's resolve_type(): set_data_type_*,
	// set_nullable, param_type_is_default, aggregate_type, fix_char_length ... in order.
	Facts []string
	// InheritsResolve is set when resolve_type() defers to a base class's.
	InheritsResolve string
	// FixFields is set when the class overrides fix_fields(): the base class's fix_fields it
	// calls, or "own" when it calls none (Item_str_func::fix_fields is where strict mode
	// makes every string function nullable, so the chain decides who inherits that).
	FixFields string
	// FixNullable is set when that fix_fields sets nullability itself, after everything
	// resolve_type and the bases decided.
	FixNullable bool
	// Header is the file that declares the class.
	Header string
}

// Catalog is everything read from the server source about functions.
type Catalog struct {
	Funcs   []Func
	Classes map[string]*ItemClass
	// FieldTypes is the index order of sql/field.cc's type tables (field_type2index): the
	// enum_field_types below the tear, then those above it, without the MYSQL_TYPE_ prefix.
	FieldTypes []string
	// MergeRules[i][j] is field_types_merge_rules[i][j], the type two values of types
	// FieldTypes[i] and FieldTypes[j] aggregate to (CASE, IF, COALESCE, GREATEST, UNION).
	MergeRules [][]string
	// ResultKinds[i] is field_types_result_type[i]: INT_RESULT, DECIMAL_RESULT, REAL_RESULT, STRING_RESULT.
	ResultKinds []string
}

var (
	reRegistryEntry = regexp.MustCompile(`\{"([A-Z_0-9]+)",\s*(SQL_[A-Z_]+)\(([^)]*)\)\}`)
	reInstantiator  = regexp.MustCompile(`(?s)class\s+([A-Za-z_0-9]+(?:_instantiator|Instantiator[A-Za-z_0-9]*))(?:<[^{]*>)?\s*\{(.*?)\n\};`)
	reUsing         = regexp.MustCompile(`(?s)(?:template\s*<[^>]*>\s*)?using\s+([A-Za-z_0-9]+)\s*=\s*([^;]+);`)
	reArgcount      = regexp.MustCompile(`static const uint (Min|Max)_argcount = (\d+);`)
	reReturnNew     = regexp.MustCompile(`return\s+new\s*\([^)]*\)\s*([A-Za-z_0-9]+)\s*\(`)
	reClassDecl     = regexp.MustCompile(`(?m)^class\s+(Item_[A-Za-z_0-9]*)\s*(?:final\s*)?:\s*public\s+([A-Za-z_0-9:<>]+)`)
	reResolveType   = regexp.MustCompile(`(?m)^bool\s+(Item_[A-Za-z_0-9]+)::resolve_type(?:_inner)?\(THD\s*\*\s*\w*\)\s*\{`)
	reFact          = regexp.MustCompile(`\b(set_data_type_[a-z_0-9]+|set_data_type|set_nullable|param_type_is_default|param_type_uses_non_param|aggregate_type|aggregate_num_type|aggregate_string_properties|agg_arg_charsets_for_string_result|agg_arg_charsets_for_comparison|fix_char_length|count_datetime_length|set_data_type_from_item|reject_geometry_args|null_on_null)\s*\(`)
	reBaseResolve   = regexp.MustCompile(`(?:if\s*\(\s*|return\s+)(Item_[A-Za-z_0-9]+)::resolve_type\(`)
	reFixFields     = regexp.MustCompile(`(?m)^bool\s+(Item_[A-Za-z_0-9]+)::fix_fields\(THD\s*\*\s*\w*,\s*Item\s*\*\*\s*\w*\)\s*\{`)
	reInlineFix     = regexp.MustCompile(`\bbool\s+fix_fields\(THD\s*\*\s*\w*,\s*Item\s*\*\*\s*\w*\)\s*override\s*\{`)
	reBaseFix       = regexp.MustCompile(`(Item_[A-Za-z_0-9]+)::fix_fields\(`)
	reBodyUnsigned  = regexp.MustCompile(`\bunsigned_flag\s*=\s*true\s*;`)
	reBodyBinary    = regexp.MustCompile(`collation\.set\(\s*&my_charset_bin`)
	reInlineResolve = regexp.MustCompile(`\bbool\s+resolve_type\(THD\s*\*\s*\w*\)\s*(?:const\s*)?override\s*\{`)
	reAssignFact    = regexp.MustCompile(`\b(max_length|decimals|unsigned_flag|collation\.set|set_nullable)\s*(=|\()\s*([^;]+);`)
)

// ReadCatalog reads the registry, the class hierarchy and the resolve_type facts.
func ReadCatalog(src string) (*Catalog, error) {
	cat := &Catalog{Classes: map[string]*ItemClass{}}
	create, err := os.ReadFile(filepath.Join(src, "sql", "item_create.cc"))
	if err != nil {
		return nil, err
	}
	createSrc := stripComments(string(create))
	instantiators := map[string]struct {
		min, max int
		class    string
	}{}
	for _, m := range reInstantiator.FindAllStringSubmatch(createSrc, -1) {
		body := m[2]
		ent := struct {
			min, max int
			class    string
		}{min: -1, max: -1}
		for _, a := range reArgcount.FindAllStringSubmatch(body, -1) {
			n, _ := strconv.Atoi(a[2])
			if a[1] == "Min" {
				ent.min = n
			} else {
				ent.max = n
			}
		}
		// the class the instantiator finally returns: the last `return new (...) Class(` in
		// the body, which wraps whatever it built before
		news := reReturnNew.FindAllStringSubmatch(body, -1)
		if len(news) > 0 {
			ent.class = news[len(news)-1][1]
		}
		instantiators[m[1]] = ent
	}
	// `using X_instantiator = Template<Class, ...>` aliases, through alias templates and
	// class aliases (I_txt = Item_func_geometry_from_text)
	aliases := map[string]string{}
	for _, m := range reUsing.FindAllStringSubmatch(createSrc, -1) {
		aliases[m[1]] = reSpace.ReplaceAllString(m[2], "")
	}
	resolveAlias := func(name string) (class string, min, max int, ok bool) {
		cur := name
		for range 8 {
			def, found := aliases[cur]
			if !found {
				break
			}
			tmpl, args := def, []string{}
			if i := strings.Index(def, "<"); i >= 0 {
				tmpl = def[:i]
				inner, _ := balanced(def, i, '<', '>')
				args = splitTopLevel(inner)
			}
			// the class: the first template argument naming an Item class, through class aliases
			for _, a := range args {
				a = strings.TrimSpace(a)
				if v, isAlias := aliases[a]; isAlias && strings.HasPrefix(v, "Item_") {
					a = v
				}
				if strings.HasPrefix(a, "Item_func") || strings.HasPrefix(a, "Item_") && !strings.Contains(a, "::") {
					class = a
					break
				}
			}
			switch {
			case tmpl == "Instantiator_with_functype" && len(args) >= 3:
				min, _ = strconv.Atoi(strings.TrimSpace(args[len(args)-1]))
				max = min
				if len(args) >= 4 { // <Class, Functype, MIN, MAX>
					min, _ = strconv.Atoi(strings.TrimSpace(args[2]))
					max, _ = strconv.Atoi(strings.TrimSpace(args[3]))
				}
				return class, min, max, class != ""
			case tmpl == "Instantiator" && len(args) >= 2:
				min, _ = strconv.Atoi(strings.TrimSpace(args[1]))
				max = min
				if len(args) >= 3 {
					max, _ = strconv.Atoi(strings.TrimSpace(args[2]))
				}
				return class, min, max, class != ""
			}
			if ent, found := instantiators[tmpl]; found { // a template class with Min/Max_argcount
				return class, ent.min, ent.max, class != ""
			}
			cur = tmpl // an alias template of another alias
		}
		return "", 0, 0, false
	}
	start := strings.Index(createSrc, "func_array[] = {")
	if start < 0 {
		return nil, fmt.Errorf("item_create.cc: no func_array")
	}
	registry, err := balanced(createSrc, strings.Index(createSrc[start:], "{")+start, '{', '}')
	if err != nil {
		return nil, err
	}
	for _, m := range reRegistryEntry.FindAllStringSubmatch(registry, -1) {
		f := Func{Name: m[1]}
		args := splitTopLevel(m[3])
		for i := range args {
			args[i] = strings.TrimSpace(args[i])
		}
		macro := m[2]
		switch macro {
		case "SQL_FACTORY":
			f.Factory = args[0]
			if ent, ok := instantiators[args[0]]; ok {
				f.Class, f.Min, f.Max = ent.class, ent.min, ent.max
			} else if class, min, max, ok := resolveAlias(args[0]); ok {
				f.Class, f.Min, f.Max = class, min, max
			} else {
				return nil, fmt.Errorf("item_create.cc: %s uses unknown instantiator %s", f.Name, args[0])
			}
		default:
			f.Class = args[0]
			f.Min, _ = strconv.Atoi(args[1])
			f.Max = f.Min
			if len(args) > 2 {
				if args[2] == "MAX_ARGLIST_SIZE" {
					f.Max = -1
				} else {
					f.Max, _ = strconv.Atoi(args[2])
				}
			}
			f.Odd = macro == "SQL_FN_ODD"
			f.Even = macro == "SQL_FN_EVEN"
			f.Internal = strings.Contains(macro, "INTERNAL")
		}
		cat.Funcs = append(cat.Funcs, f)
	}

	// class hierarchy from the headers
	headers, _ := filepath.Glob(filepath.Join(src, "sql", "item*.h"))
	sort.Strings(headers)
	for _, h := range headers {
		b, err := os.ReadFile(h)
		if err != nil {
			return nil, err
		}
		text := stripComments(string(b))
		for _, m := range reClassDecl.FindAllStringSubmatchIndex(text, -1) {
			name, base := text[m[2]:m[3]], text[m[4]:m[5]]
			if _, dup := cat.Classes[name]; dup {
				continue
			}
			c := &ItemClass{Name: name, Base: base, Header: filepath.Base(h)}
			cat.Classes[name] = c
			// a resolve_type defined inline in the class body (109 of them in 8.4) reads the
			// same way as one in a .cc file
			open := strings.IndexByte(text[m[1]:], '{')
			if open < 0 {
				continue
			}
			class, err := balanced(text, m[1]+open, '{', '}')
			if err != nil {
				continue
			}
			if im := reInlineResolve.FindStringIndex(class); im != nil {
				if body, err := balanced(class, im[1]-1, '{', '}'); err == nil {
					c.readFacts(body)
				}
			}
			if im := reInlineFix.FindStringIndex(class); im != nil {
				if body, err := balanced(class, im[1]-1, '{', '}'); err == nil {
					c.readFixFields(body)
				}
			}
			// what a constructor states about the result is a fact too (set_nullable(true) in
			// MAKEDATE's, unsigned_flag = true in CRC32's)
			ctor := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\s*\((?:[^()]|\([^()]*\))*\)\s*(?::[^{;]*)?\{`)
			for _, cm := range ctor.FindAllStringIndex(class, -1) {
				if body, err := balanced(class, cm[1]-1, '{', '}'); err == nil {
					c.readFacts(body)
				}
			}
			if reBodyUnsigned.MatchString(class) {
				c.Facts = append(c.Facts, "unsigned_flag=true")
			}
			if reBodyBinary.MatchString(class) {
				c.Facts = append(c.Facts, "collation.set=&my_charset_bin)")
			}
		}
	}
	for _, c := range cat.Classes {
		c.Family = cat.family(c.Name)
	}

	// resolve_type facts from the implementations
	sources, _ := filepath.Glob(filepath.Join(src, "sql", "item*.cc"))
	sort.Strings(sources)
	for _, f := range sources {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		text := stripComments(string(b))
		for _, m := range reResolveType.FindAllStringSubmatchIndex(text, -1) {
			name := text[m[2]:m[3]]
			body, err := balanced(text, m[1]-1, '{', '}')
			if err != nil {
				continue
			}
			c := cat.Classes[name]
			if c == nil {
				c = &ItemClass{Name: name}
				cat.Classes[name] = c
			}
			c.readFacts(body)
		}
		for _, m := range reFixFields.FindAllStringSubmatchIndex(text, -1) {
			name := text[m[2]:m[3]]
			body, err := balanced(text, m[1]-1, '{', '}')
			if err != nil {
				continue
			}
			c := cat.Classes[name]
			if c == nil {
				c = &ItemClass{Name: name}
				cat.Classes[name] = c
			}
			c.readFixFields(body)
		}
	}
	if err := readTypeTables(src, cat); err != nil {
		return nil, err
	}
	return cat, nil
}

// families are the bases that fix a result kind; the walk up the hierarchy stops at the
// first of them.
var families = map[string]bool{
	"Item_int_func": true, "Item_bool_func": true, "Item_str_func": true, "Item_str_ascii_func": true,
	"Item_real_func": true, "Item_dec_func": true, "Item_func_numhybrid": true, "Item_func_num1": true, "Item_num_op": true,
	"Item_datetime_func": true, "Item_date_func": true, "Item_time_func": true, "Item_temporal_func": true, "Item_temporal_hybrid_func": true,
	"Item_json_func": true, "Item_geometry_func": true, "Item_func_bit": true,
	"Item_sum_num": true, "Item_sum_int": true, "Item_sum_sum": true, "Item_sum_bit": true, "Item_sum_hybrid": true, "Item_sum": true,
	"Item_non_framing_wf": true, "Item_func": true,
	"Item_static_string_func": true, "Item_float": true, "Item_int": true, "Item_string": true,
}

func (cat *Catalog) family(name string) string {
	seen := map[string]bool{}
	for cur := name; cur != "" && !seen[cur]; {
		seen[cur] = true
		if families[cur] && cur != name {
			return cur
		}
		c := cat.Classes[cur]
		if c == nil {
			return ""
		}
		cur = c.Base
		if i := strings.Index(cur, "<"); i >= 0 {
			cur = cur[:i]
		}
	}
	return ""
}

// Report summarizes the catalog read: how many functions, how many resolve to a class
// with a family, and the classes without one.
func (cat *Catalog) Report() string {
	var b strings.Builder
	noClass, noFamily := 0, 0
	var missing []string
	for _, f := range cat.Funcs {
		c := cat.Classes[f.Class]
		switch {
		case c == nil:
			noClass++
			missing = append(missing, f.Name+"→"+f.Class+" (no class)")
		case c.Family == "":
			noFamily++
			missing = append(missing, f.Name+"→"+f.Class+" (base "+c.Base+")")
		}
	}
	withFacts := 0
	for _, c := range cat.Classes {
		if len(c.Facts) > 0 {
			withFacts++
		}
	}
	fmt.Fprintf(&b, "registry: %d functions; %d without a known class, %d whose class has no family; %d Item classes, %d with resolve_type facts\n",
		len(cat.Funcs), noClass, noFamily, len(cat.Classes), withFacts)
	sort.Strings(missing)
	for _, m := range missing {
		fmt.Fprintf(&b, "  %s\n", m)
	}
	fam := map[string]int{}
	for _, f := range cat.Funcs {
		if c := cat.Classes[f.Class]; c != nil {
			fam[c.Family]++
		}
	}
	for _, kv := range top(fam, 30) {
		fmt.Fprintf(&b, "  %4d  %s\n", kv.n, kv.k)
	}
	return b.String()
}

// CatalogGo is the Go source of the function catalog: the registry entries with their
// classes, and what the headers say about every class the registry or the grammar builds
// (its family, its resolve_type facts), for package catalog to interpret.
func CatalogGo(pkg, version string, cat *Catalog, grammarClasses []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "// Code generated by parsegen from MySQL %s sql/item_create.cc and sql/item*. DO NOT EDIT.\n\npackage %s\n\n", version, pkg)
	b.WriteString("// Functions is the native function registry (sql/item_create.cc func_array), in the server's order.\nvar Functions = [...]Function{\n")
	for _, f := range cat.Funcs {
		fmt.Fprintf(&b, "\t{Name: %s, Class: %s, Min: %d, Max: %d", strconv.Quote(f.Name), strconv.Quote(f.Class), f.Min, f.Max)
		if f.Odd {
			b.WriteString(", Odd: true")
		}
		if f.Even {
			b.WriteString(", Even: true")
		}
		if f.Internal {
			b.WriteString(", Internal: true")
		}
		if f.Factory != "" {
			fmt.Fprintf(&b, ", Factory: %s", strconv.Quote(f.Factory))
		}
		b.WriteString("},\n")
	}
	b.WriteString("}\n\n")
	// classes: the registry's, the grammar's, and every base on their way up
	want := map[string]bool{}
	var add func(string)
	add = func(name string) {
		for cur := name; cur != "" && !want[cur]; {
			want[cur] = true
			c := cat.Classes[cur]
			if c == nil {
				return
			}
			cur = c.Base
			if i := strings.Index(cur, "<"); i >= 0 {
				cur = cur[:i]
			}
		}
	}
	for _, f := range cat.Funcs {
		add(f.Class)
	}
	for _, c := range grammarClasses {
		add(c)
	}
	var names []string
	for n := range want {
		if cat.Classes[n] != nil {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	b.WriteString("// Items is what the server declares about each Item class: its base, the family that\n// fixes its result kind, and the declarative facts of its resolve_type().\nvar Items = map[string]Item{\n")
	for _, n := range names {
		c := cat.Classes[n]
		fmt.Fprintf(&b, "\t%s: {Base: %s, Family: %s", strconv.Quote(n), strconv.Quote(c.Base), strconv.Quote(c.Family))
		if c.InheritsResolve != "" {
			fmt.Fprintf(&b, ", InheritsResolve: %s", strconv.Quote(c.InheritsResolve))
		}
		if c.FixFields != "" {
			fmt.Fprintf(&b, ", FixFields: %s", strconv.Quote(c.FixFields))
		}
		if c.FixNullable {
			b.WriteString(", FixNullable: true")
		}
		if len(c.Facts) > 0 {
			b.WriteString(", Facts: []string{")
			for i, f := range c.Facts {
				if i > 0 {
					b.WriteString(", ")
				}
				b.WriteString(strconv.Quote(f))
			}
			b.WriteString("}")
		}
		b.WriteString("},\n")
	}
	b.WriteString("}\n\n")
	b.WriteString("// FieldTypes is the index order of MergeRules and ResultKinds (sql/field.cc field_type2index):\n// the enum_field_types names without their MYSQL_TYPE_ prefix.\nvar FieldTypes = [...]string{")
	for i, t := range cat.FieldTypes {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(strconv.Quote(t))
	}
	b.WriteString("}\n\n")
	fmt.Fprintf(&b, "// MergeRules is field_types_merge_rules: the type two values of types FieldTypes[i] and\n// FieldTypes[j] aggregate to, as CASE, IF, COALESCE, GREATEST and UNION do.\nvar MergeRules = [...][%d]string{\n", len(cat.FieldTypes))
	for i, row := range cat.MergeRules {
		fmt.Fprintf(&b, "\t/* %s */ {", cat.FieldTypes[i])
		for j, t := range row {
			if j > 0 {
				b.WriteString(", ")
			}
			b.WriteString(strconv.Quote(t))
		}
		b.WriteString("},\n")
	}
	b.WriteString("}\n\n")
	b.WriteString("// ResultKinds is field_types_result_type: the Item_result of each of FieldTypes.\nvar ResultKinds = [...]string{")
	for i, k := range cat.ResultKinds {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(strconv.Quote(k))
	}
	b.WriteString("}\n")
	return b.String()
}

var (
	reEnumMember = regexp.MustCompile(`(?m)^\s*MYSQL_TYPE_([A-Z_0-9]+)(?:\s*=\s*(\d+))?\s*,?`)
	reTypeToken  = regexp.MustCompile(`MYSQL_TYPE_([A-Z_0-9]+)|\b((?:INT|DECIMAL|REAL|STRING|ROW|INVALID)_RESULT)\b`)
)

// readTypeTables reads include/field_types.h for the enum order and sql/field.cc for the
// two tables indexed by field_type2index: field_types_merge_rules (what two types
// aggregate to) and field_types_result_type (the result kind of a type). The tear in the
// enum (17..242 unused) is what field_type2index folds; FIELDTYPE_NUM follows from it.
func readTypeTables(src string, cat *Catalog) error {
	hdr, err := os.ReadFile(filepath.Join(src, "include", "field_types.h"))
	if err != nil {
		return err
	}
	body := stripComments(string(hdr))
	start := strings.Index(body, "enum enum_field_types {")
	end := strings.Index(body[start:], "};")
	if start < 0 || end < 0 {
		return fmt.Errorf("catalog: enum_field_types not found in field_types.h")
	}
	type member struct {
		name string
		val  int
	}
	var members []member
	next := 0
	for _, m := range reEnumMember.FindAllStringSubmatch(body[start:start+end], -1) {
		if m[2] != "" {
			next, _ = strconv.Atoi(m[2])
		}
		members = append(members, member{m[1], next})
		next++
	}
	fieldCC, err := os.ReadFile(filepath.Join(src, "sql", "field.cc"))
	if err != nil {
		return err
	}
	fsrc := stripComments(string(fieldCC))
	tearFrom, tearTo := -1, -1
	if m := regexp.MustCompile(`#define FIELDTYPE_TEAR_FROM \(MYSQL_TYPE_([A-Z_0-9]+) \+ 1\)`).FindStringSubmatch(fsrc); m != nil {
		for _, mem := range members {
			if mem.name == m[1] {
				tearFrom = mem.val + 1
			}
		}
	}
	if m := regexp.MustCompile(`#define FIELDTYPE_TEAR_TO \((\d+) - 1\)`).FindStringSubmatch(fsrc); m != nil {
		n, _ := strconv.Atoi(m[1])
		tearTo = n - 1
	}
	if tearFrom < 0 || tearTo < 0 {
		return fmt.Errorf("catalog: FIELDTYPE_TEAR_FROM / TEAR_TO not found in field.cc")
	}
	for _, mem := range members {
		if mem.val < tearFrom || mem.val > tearTo {
			cat.FieldTypes = append(cat.FieldTypes, mem.name)
		}
	}
	n := len(cat.FieldTypes)
	table := func(decl string) ([]string, error) {
		i := strings.Index(fsrc, decl)
		if i < 0 {
			return nil, fmt.Errorf("catalog: %s not found in field.cc", decl)
		}
		j := strings.Index(fsrc[i:], "};")
		var out []string
		for _, m := range reTypeToken.FindAllStringSubmatch(fsrc[i+len(decl):i+j], -1) {
			if m[1] != "" {
				out = append(out, m[1])
			} else {
				out = append(out, m[2])
			}
		}
		return out, nil
	}
	merge, err := table("field_types_merge_rules[FIELDTYPE_NUM][FIELDTYPE_NUM] =")
	if err != nil {
		return err
	}
	if len(merge) != n*n {
		return fmt.Errorf("catalog: field_types_merge_rules has %d entries, want %d×%d", len(merge), n, n)
	}
	for i := 0; i < n; i++ {
		cat.MergeRules = append(cat.MergeRules, merge[i*n:(i+1)*n])
	}
	kinds, err := table("field_types_result_type[FIELDTYPE_NUM] =")
	if err != nil {
		return err
	}
	if len(kinds) != n {
		return fmt.Errorf("catalog: field_types_result_type has %d entries, want %d", len(kinds), n)
	}
	cat.ResultKinds = kinds
	return nil
}

// readFixFields records a fix_fields override: the base fix_fields it calls, or "own".
func (c *ItemClass) readFixFields(body string) {
	c.FixFields = "own"
	if m := reBaseFix.FindStringSubmatch(body); m != nil {
		c.FixFields = m[1]
	}
	// an unconditional set_nullable(true / false) is a fact; anything else computed there
	// is the override deciding for itself
	switch {
	case strings.Contains(body, "set_nullable(true)"):
		c.Facts = append(c.Facts, "set_nullable(true)")
	case strings.Contains(body, "set_nullable(false)"):
		c.Facts = append(c.Facts, "set_nullable(false)")
	case strings.Contains(body, "set_nullable("):
		c.FixNullable = true
	}
}

// readFacts reads the declarative statements of a resolve_type body into the class's
// facts, and the base class whose resolve_type it calls first.
func (c *ItemClass) readFacts(body string) {
	if bm := reBaseResolve.FindStringSubmatch(body); bm != nil {
		c.InheritsResolve = bm[1]
	}
	for _, fm := range reFact.FindAllStringSubmatchIndex(body, -1) {
		call, err := balanced(body, fm[1]-1, '(', ')')
		if err != nil {
			continue
		}
		c.Facts = append(c.Facts, body[fm[2]:fm[3]]+"("+reSpace.ReplaceAllString(call, "")+")")
	}
	for _, am := range reAssignFact.FindAllStringSubmatch(body, -1) {
		if am[1] == "set_nullable" {
			continue // already a call fact
		}
		c.Facts = append(c.Facts, am[1]+"="+reSpace.ReplaceAllString(am[3], ""))
	}
}
