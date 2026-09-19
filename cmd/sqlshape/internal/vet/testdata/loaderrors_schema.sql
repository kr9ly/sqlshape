-- sqlshape: postgres 17
-- a schema-level problem (s.Problems), unrelated to any function / policy / view body
CREATE TABLE dup_table (id bigint PRIMARY KEY);
CREATE TABLE dup_table (id bigint PRIMARY KEY);

CREATE DOMAIN yen AS bigint;
CREATE DOMAIN usd AS bigint;

CREATE TABLE t (
    id bigint PRIMARY KEY,
    a  yen,
    c  usd
);
ALTER TABLE t ENABLE ROW LEVEL SECURITY;

-- a hard error: aggregates are not allowed in policy predicates
CREATE POLICY p_bad ON t USING (count(*) > 0);

-- a non-advisory note: mixing two different domains in a policy predicate
CREATE POLICY p_note ON t USING (a > c);

-- a hard error: a function body referencing a relation that does not exist
CREATE FUNCTION bad_fn() RETURNS bigint LANGUAGE sql AS $$ SELECT count(*) FROM nonexistent_table $$;

-- a non-advisory note: mixing two different domains
CREATE FUNCTION note_fn() RETURNS boolean LANGUAGE sql AS $$ SELECT a > c FROM t $$;

-- a hard error: a view referencing a relation that does not exist
CREATE VIEW bad_view AS SELECT * FROM nonexistent_table2;

-- a non-advisory note: mixing two different domains
CREATE VIEW note_view AS SELECT a, c FROM t WHERE a > c;
