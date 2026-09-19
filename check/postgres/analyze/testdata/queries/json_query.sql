SELECT JSON_EXISTS(meta, '$.a') AS ex, JSON_QUERY(meta, '$.a') AS q, JSON_QUERY(meta, '$.a' RETURNING json WITH WRAPPER) AS qw,
       JSON_VALUE(meta, '$.a') AS v, JSON_VALUE(meta, '$.n' RETURNING int DEFAULT 0 ON EMPTY ERROR ON ERROR) AS vi,
       JSON_VALUE(meta, '$.x ? (@ > $lim)' PASSING $1 AS lim RETURNING numeric) AS vp
FROM orders
