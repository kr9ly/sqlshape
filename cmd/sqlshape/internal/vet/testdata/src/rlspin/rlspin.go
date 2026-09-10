package rlspin

import "github.com/kr9ly/sqlshape/v2"

// -require-columns=tenant_id with -strict: a row-level security policy that fixes the
// column pins it; when row security is not forced the pin holds for non-owner roles only.

type TenantID string // want TenantID:`bound k tenants.id`

var sealed = sqlshape.Query[int64, struct{}](`SELECT count(*) FROM sealed`) // want `schema .*: drafts has policies but row level security is not enabled` `schema: vault has row level security enabled and no policy` `schema: policy docs_tenant on docs reads current_setting` `schema: function all_docs is SECURITY DEFINER`

var docs = sqlshape.Query[int64, struct{}](`SELECT count(*) FROM docs`) // want `docs.tenant_id is pinned by policy docs_tenant for roles subject to row security, not for the table's owner: FORCE ROW LEVEL SECURITY if the application connects as the owner`

var drafts = sqlshape.Query[int64, struct{}](`SELECT count(*) FROM drafts`) // want `drafts.tenant_id is not pinned: every statement on drafts must fix tenant_id by equality`

var pinnedAnyway = sqlshape.Query[int64, struct{ T TenantID }](`SELECT count(*) FROM docs WHERE tenant_id = {{.T}}`)

// andConjuncts: the pin may sit anywhere among AND'ed conjuncts, not just first, and nested ANDs flatten too
var andPinned = sqlshape.Query[int64, struct{}](`SELECT count(*) FROM andpin`)

var nestedPinned = sqlshape.Query[int64, struct{}](`SELECT count(*) FROM nestedpin`)

// an OR does not pin: neither disjunct alone fixes tenant_id for every row
var orNotPinned = sqlshape.Query[int64, struct{}](`SELECT count(*) FROM orpin`) // want `orpin.tenant_id is not pinned: every statement on orpin must fix tenant_id by equality \(or assign it\)`

// pinsColumn reads the equality either way round: expr = col pins just as col = expr does
var revPinned = sqlshape.Query[int64, struct{}](`SELECT count(*) FROM revpin`)

// a policy scoped to UPDATE does not pin a SELECT, even though its predicate matches
var updateOnlyNotPinned = sqlshape.Query[int64, struct{}](`SELECT count(*) FROM updateonly`) // want `updateonly.tenant_id is not pinned: every statement on updateonly must fix tenant_id by equality \(or assign it\)`
