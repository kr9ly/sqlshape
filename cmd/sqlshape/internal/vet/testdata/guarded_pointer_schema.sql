-- sqlshape: postgres 17
-- m12e-guarded-pointer: a plain {{if .X}} / {{with .X}} that takes the then branch
-- proves a pointer field non-nil for the rest of that expansion; the checker should not
-- report a NOT NULL violation for a parameter read that way.
CREATE TABLE t (
    id    bigint PRIMARY KEY,
    name  text NOT NULL DEFAULT 'x',
    extra text NOT NULL DEFAULT 'y'
);
