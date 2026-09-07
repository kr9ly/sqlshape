package dto

import "github.com/kr9ly/sqlshape"

// A fresh struct covering the reverse mapping's exotic branches: network / hardware
// address types, an interval, hstore, a range and multirange, geometry, tsvector, and
// the pg_catalog string-fallback types (xml, money, timetz, oid).
type HostRow struct{}

var hostRow = sqlshape.Query[HostRow, struct{}](`SELECT addr, net, mac, uptime, attrs, span, spans, seen, pos, doc, body, fee, at_tz, rel FROM hosts`) // want `result column "addr" has no field` `result column "net" has no field` `result column "mac" has no field` `result column "uptime" has no field` `result column "attrs" has no field` `result column "span" has no field` `result column "spans" has no field` `result column "seen" has no field` `result column "pos" has no field` `result column "doc" has no field` `result column "body" has no field` `result column "fee" has no field` `result column "at_tz" has no field` `result column "rel" has no field`
