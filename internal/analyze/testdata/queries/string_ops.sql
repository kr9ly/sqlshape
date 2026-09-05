SELECT lower(email) AS e, name || '!' AS greet, length(name) AS len, 'x' || 'y' AS xy, upper($1) AS up FROM users
