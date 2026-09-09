package analyze

import (
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/kr9ly/sqlshape/internal/pgparse"
)

// equalIgnoringLocation reports whether two parse-tree nodes are the same expression,
// wherever each was written (the same `b + 1` in DISTINCT ON and in ORDER BY).
func equalIgnoringLocation(x, y *pgparse.Node) bool {
	if x == nil || y == nil {
		return x == y
	}
	cx, cy := proto.Clone(x), proto.Clone(y)
	clearLocations(cx.ProtoReflect())
	clearLocations(cy.ProtoReflect())
	return proto.Equal(cx, cy)
}

// clearLocations zeroes every `location` field of m, recursively.
func clearLocations(m protoreflect.Message) {
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.Name() == "location" && fd.Kind() == protoreflect.Int32Kind:
			m.Clear(fd)
		case fd.IsList() && fd.Kind() == protoreflect.MessageKind:
			l := v.List()
			for i := 0; i < l.Len(); i++ {
				clearLocations(l.Get(i).Message())
			}
		case fd.Kind() == protoreflect.MessageKind && !fd.IsMap():
			clearLocations(v.Message())
		}
		return true
	})
}
