SELECT tags, tags[1] AS first, array_length(tags, 1) AS n, tags || $1 AS more, array_append(tags, $2) AS app, ARRAY[1, 2, 3] AS ints, ARRAY['a', 'b'] AS strs, unnest(tags) AS tag FROM users
