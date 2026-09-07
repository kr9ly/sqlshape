CREATE TABLE tenants (id uuid PRIMARY KEY);

-- policies on a table whose row security is enabled, one reading a setting with missing_ok
CREATE TABLE docs (id bigint PRIMARY KEY, tenant_id uuid NOT NULL REFERENCES tenants, body text);
ALTER TABLE docs ENABLE ROW LEVEL SECURITY;
CREATE POLICY docs_tenant ON docs USING (tenant_id = current_setting('app.tenant', true)::uuid);

-- policies without ENABLE: a schema problem
CREATE TABLE drafts (id bigint PRIMARY KEY, tenant_id uuid NOT NULL);
CREATE POLICY drafts_tenant ON drafts USING (tenant_id = current_setting('app.tenant')::uuid);

-- row security on, no policy: default deny
CREATE TABLE vault (id bigint PRIMARY KEY);
ALTER TABLE vault ENABLE ROW LEVEL SECURITY;

-- a SECURITY DEFINER function past the policies
CREATE FUNCTION all_docs() RETURNS SETOF docs LANGUAGE sql SECURITY DEFINER STABLE RETURN (SELECT docs FROM docs);
CREATE FUNCTION count_docs() RETURNS bigint LANGUAGE sql STABLE RETURN (SELECT count(*) FROM docs);
