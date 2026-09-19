SELECT u.id, x.tag, x.m FROM users u CROSS JOIN LATERAL unnest(u.tags, ARRAY[1, 2]) AS x(tag, m)
