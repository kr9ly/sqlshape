SELECT attrs -> 'k' AS v, attrs ? $1 AS has, akeys(attrs) AS ks, hstore('a', 'b') || attrs AS merged, (attrs -> ARRAY['a', 'b'])[1] AS first
FROM users WHERE attrs @> 'k=>v'
