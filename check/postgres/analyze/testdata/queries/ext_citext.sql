SELECT handle, lower(handle) AS lo, handle || 'x' AS cat, handle::text AS t
FROM users WHERE handle = 'Alice' AND handle LIKE $1 AND handle <> name
