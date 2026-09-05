SELECT meta, meta->'a' AS a, meta->>'b' AS b, meta @> $1 AS contains, jsonb_build_object('id', id, 'total', total) AS obj, to_jsonb(status) AS js FROM orders
