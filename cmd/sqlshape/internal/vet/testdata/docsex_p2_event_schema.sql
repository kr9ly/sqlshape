-- sqlshape: postgres 17
-- "Do not receive with a type that loses information (-strict)": an events table with a
-- plain timestamp and a date column, neither nullable (so the only advisories are the
-- ones the doc's Event struct itself calls out).
CREATE TABLE events (
    at  timestamp without time zone NOT NULL,
    day date NOT NULL
);
