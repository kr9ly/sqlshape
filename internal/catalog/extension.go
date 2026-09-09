package catalog

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Extensions.
//
// An extension's catalog is dumped by ./gen -ext <name> from a fresh database right
// after CREATE EXTENSION ... CASCADE, so a dump is a bundle: the extension and whatever
// it required (META lists them). The OIDs in a dump are the ones that database handed
// out (from FirstNormalObjectID up), so they are neither stable nor disjoint between
// extensions; WithExtensions renumbers them into a range of their own on load, keeping
// user objects (FirstUserOID and up) and the bootstrap catalog clear of them.

// Extension describes one dumped extension.
type Extension struct {
	Name     string
	Version  string
	Schema   string   // the namespace its objects were created in
	Requires []string // extensions the dump bundles
}

// FirstNormalObjectID is where PG starts handing out OIDs for user objects.
const FirstNormalObjectID OID = 16384

// extBase is where the renumbered OIDs of the i-th loaded extension start.
func extBase(i int) OID { return 1<<28 + OID(i)<<20 }

// Available lists the extensions with an embedded dump for a PostgreSQL major version.
func Available(major int) []string {
	entries, err := data.ReadDir("data/" + strconv.Itoa(major) + "/ext")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// ReadExtension returns the metadata of a dumped extension for a PostgreSQL major
// version, or an error if there is no dump.
func ReadExtension(major int, name string) (*Extension, error) {
	b, err := data.ReadFile(fmt.Sprintf("data/%d/ext/%s/META", major, name))
	if err != nil {
		return nil, fmt.Errorf("extension %q: no dumped catalog for PostgreSQL %d (run: go run ./internal/catalog/gen -pg %d -ext %s)", name, major, major, name)
	}
	e := &Extension{Name: name}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		k, v, _ := strings.Cut(line, "\t")
		switch k {
		case "version":
			e.Version = v
		case "schema":
			e.Schema = v
		case "requires":
			if v != "" {
				e.Requires = strings.Split(v, ",")
			}
		}
	}
	return e, nil
}

// WithExtensions returns a new catalog: this one plus the named extensions. An extension
// another requested one already bundles is not loaded twice. Objects are renumbered into
// a per-extension OID range; the namespace they were created in is kept (Type.Schema etc.).
func (c *Catalog) WithExtensions(names []string) (*Catalog, error) {
	var exts []*Extension
	for _, n := range names {
		e, err := ReadExtension(c.Major, n)
		if err != nil {
			return nil, err
		}
		exts = append(exts, e)
	}
	bundled := map[string]bool{}
	for _, e := range exts {
		for _, r := range e.Requires {
			bundled[r] = true
		}
	}
	out := newCatalog()
	out.Major = c.Major
	out.Version = c.Version
	out.Types = append(out.Types, c.Types...)
	out.Funcs = append(out.Funcs, c.Funcs...)
	out.Operators = append(out.Operators, c.Operators...)
	out.Casts = append(out.Casts, c.Casts...)
	out.Aggregates = append(out.Aggregates, c.Aggregates...)
	out.Ranges = append(out.Ranges, c.Ranges...)
	seen := map[string]bool{}
	for _, e := range exts {
		if bundled[e.Name] || seen[e.Name] {
			continue
		}
		seen[e.Name] = true
		i := len(out.Extensions)
		out.Extensions = append(out.Extensions, e)
		next := extBase(i)
		m := map[OID]OID{}
		remap := func(o OID) OID {
			if o < FirstNormalObjectID {
				return o
			}
			if r, ok := m[o]; ok {
				return r
			}
			m[o] = next
			next++
			return m[o]
		}
		if err := out.load(fmt.Sprintf("data/%d/ext/%s", c.Major, e.Name), "", remap); err != nil {
			return nil, fmt.Errorf("extension %s: %w", e.Name, err)
		}
	}
	out.index()
	return out, nil
}
