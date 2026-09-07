package rls

import "github.com/kr9ly/sqlshape"

// Row-level security: the schema problems and the -strict advisories about it are
// reported once, on the first query of the package.
var docs = sqlshape.Query[*string, struct{}](`SELECT body FROM docs`) // want `schema .*: drafts has policies but row level security is not enabled, so they do not apply: ALTER TABLE drafts ENABLE ROW LEVEL SECURITY` `schema: vault has row level security enabled and no policy: every role but the owner sees no rows` `schema: policy docs_tenant on docs reads current_setting\("app.tenant", true\): a session that never set it gets NULL` `schema: function all_docs is SECURITY DEFINER and reaches docs, whose policies do not bind the owner`
