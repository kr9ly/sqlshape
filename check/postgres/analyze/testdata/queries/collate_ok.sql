SELECT name, alias, name || alias AS both, alias COLLATE "POSIX" AS posix, lower(alias) AS lo
FROM users
WHERE name = alias AND alias LIKE $1 AND name < ('x' COLLATE "C")
ORDER BY alias, name COLLATE "C"
