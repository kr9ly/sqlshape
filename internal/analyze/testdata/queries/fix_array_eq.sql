SELECT ARRAY[1::bigint] = ARRAY[2::bigint] AS eq, (SELECT array_agg(id) FROM orders) <> ARRAY[1::bigint] AS ne, ARRAY[id] @> ARRAY[$1::bigint] AS has, ARRAY[1, 2] = '{1,2}' AS lit
FROM orders
