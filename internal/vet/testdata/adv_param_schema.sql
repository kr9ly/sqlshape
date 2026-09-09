-- sqlshape: postgres 17
CREATE TABLE t (
    id     bigint PRIMARY KEY,
    tags   smallint[],
    reals  real[],
    small  smallint NOT NULL
);
