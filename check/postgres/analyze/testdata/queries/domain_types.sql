SELECT balance, balance + 1 AS b1, balance * 2 AS b2, email, lower(email) AS le FROM users WHERE balance > $1 AND email = $2
