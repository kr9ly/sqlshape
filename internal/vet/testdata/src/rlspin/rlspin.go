package rlspin

import "github.com/kr9ly/sqlshape"

// -require-columns=tenant_id with -strict: a row-level security policy that fixes the
// column pins it; when row security is not forced the pin holds for non-owner roles only.

type TenantID string // want TenantID:`bound k tenants.id`

var sealed = sqlshape.Query[int64, struct{}](`SELECT count(*) FROM sealed`) // want `schema .*: drafts has policies but row level security is not enabled` `schema: vault has row level security enabled and no policy` `schema: policy docs_tenant on docs reads current_setting` `schema: function all_docs is SECURITY DEFINER`

var docs = sqlshape.Query[int64, struct{}](`SELECT count(*) FROM docs`) // want `docs.tenant_id is pinned by policy docs_tenant for roles subject to row security, not for the table's owner: FORCE ROW LEVEL SECURITY if the application connects as the owner`

var drafts = sqlshape.Query[int64, struct{}](`SELECT count(*) FROM drafts`) // want `drafts.tenant_id is not pinned: every statement on drafts must fix tenant_id by equality`

var pinnedAnyway = sqlshape.Query[int64, struct{ T TenantID }](`SELECT count(*) FROM docs WHERE tenant_id = {{.T}}`)
