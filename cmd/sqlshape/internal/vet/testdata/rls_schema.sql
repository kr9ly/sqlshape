-- sqlshape: postgres 17
CREATE TABLE tenants (id uuid PRIMARY KEY);

-- policies on a table whose row security is enabled, one reading a setting with missing_ok
CREATE TABLE docs (id bigint PRIMARY KEY, tenant_id uuid NOT NULL REFERENCES tenants, body text);
ALTER TABLE docs ENABLE ROW LEVEL SECURITY;
CREATE POLICY docs_tenant ON docs USING (tenant_id = current_setting('app.tenant', true)::uuid);
CREATE INDEX docs_tenant_idx ON docs (tenant_id);

-- policies without ENABLE: a schema problem
CREATE TABLE drafts (id bigint PRIMARY KEY, tenant_id uuid NOT NULL);
CREATE POLICY drafts_tenant ON drafts USING (tenant_id = current_setting('app.tenant')::uuid);

-- row security on, no policy: default deny
CREATE TABLE vault (id bigint PRIMARY KEY);
ALTER TABLE vault ENABLE ROW LEVEL SECURITY;

-- a SECURITY DEFINER function past the policies
CREATE FUNCTION all_docs() RETURNS SETOF docs LANGUAGE sql SECURITY DEFINER STABLE RETURN (SELECT docs FROM docs);
CREATE FUNCTION count_docs() RETURNS bigint LANGUAGE sql STABLE RETURN (SELECT count(*) FROM docs);

-- row security forced: the policy pins tenant_id for every role, the owner included
CREATE TABLE sealed (id bigint PRIMARY KEY, tenant_id uuid NOT NULL);
ALTER TABLE sealed ENABLE ROW LEVEL SECURITY;
ALTER TABLE sealed FORCE ROW LEVEL SECURITY;
CREATE POLICY sealed_tenant ON sealed USING (tenant_id = current_setting('app.tenant')::uuid);

-- conjunctions: the pin can sit anywhere among AND'ed conjuncts, nested or not
CREATE TABLE andpin (id bigint PRIMARY KEY, region text NOT NULL, tenant_id uuid NOT NULL);
ALTER TABLE andpin ENABLE ROW LEVEL SECURITY;
ALTER TABLE andpin FORCE ROW LEVEL SECURITY;
CREATE POLICY andpin_tenant ON andpin USING (region = 'us' AND tenant_id = current_setting('app.tenant')::uuid);

CREATE TABLE nestedpin (id bigint PRIMARY KEY, region text NOT NULL, tenant_id uuid NOT NULL, flag boolean NOT NULL);
ALTER TABLE nestedpin ENABLE ROW LEVEL SECURITY;
ALTER TABLE nestedpin FORCE ROW LEVEL SECURITY;
CREATE POLICY nestedpin_tenant ON nestedpin USING ((region = 'us' AND tenant_id = current_setting('app.tenant')::uuid) AND flag = true);

-- an OR does not pin: either side alone can pass, so tenant_id is not fixed for every row
CREATE TABLE orpin (id bigint PRIMARY KEY, tenant_id uuid NOT NULL);
ALTER TABLE orpin ENABLE ROW LEVEL SECURITY;
ALTER TABLE orpin FORCE ROW LEVEL SECURITY;
CREATE POLICY orpin_tenant ON orpin USING (tenant_id = current_setting('app.tenant')::uuid OR true);

-- the equality can read either way round: expr = col pins just as col = expr does
CREATE TABLE revpin (id bigint PRIMARY KEY, tenant_id uuid NOT NULL);
ALTER TABLE revpin ENABLE ROW LEVEL SECURITY;
ALTER TABLE revpin FORCE ROW LEVEL SECURITY;
CREATE POLICY revpin_tenant ON revpin USING (current_setting('app.tenant')::uuid = tenant_id);

-- a policy that does not apply to SELECT does not pin reads, even with a matching predicate
CREATE TABLE updateonly (id bigint PRIMARY KEY, tenant_id uuid NOT NULL);
ALTER TABLE updateonly ENABLE ROW LEVEL SECURITY;
ALTER TABLE updateonly FORCE ROW LEVEL SECURITY;
CREATE POLICY updateonly_tenant ON updateonly FOR UPDATE USING (tenant_id = current_setting('app.tenant')::uuid);
