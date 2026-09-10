package docs

import "github.com/kr9ly/sqlshape"

// A second file: fieldDecl / typeSpec must skip the first file (whose range does not
// contain this type's position) before finding the match here.

type OtherFile struct { // want `type OtherFile has no doc comment; the schema says: One purchase\.`
	ID     int64
	Status OrderStatus // want `field Status has no doc comment; the schema says: Lifecycle state; see order_status\.`
}

var otherFile = sqlshape.Query[OtherFile, struct{}](`SELECT id, status FROM orders`)
