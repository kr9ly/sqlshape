package rls

import "github.com/kr9ly/sqlshape/v2"

// Row-level security: the schema problems (always reported) and the -strict advisories
// about a relation (reported only for a package that actually touches that relation --
// vault below is claimed because vaultCount reads vault, not merely because -strict is
// on) are all attached to the first query of the package.
var docs = sqlshape.Query[*string, struct{}](`SELECT body FROM docs`) // want `schema .*: drafts has policies but row level security is not enabled, so they do not apply: ALTER TABLE drafts ENABLE ROW LEVEL SECURITY` `schema: vault has row level security enabled and no policy: every role but the owner sees no rows` `schema: policy docs_tenant on docs reads current_setting\("app.tenant", true\): a session that never set it gets NULL` `schema: function all_docs is SECURITY DEFINER and reaches docs, whose policies do not bind the owner`

var vaultCount = sqlshape.Query[int64, struct{}](`SELECT count(*) FROM vault`)
