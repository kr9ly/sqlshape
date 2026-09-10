SELECT role, yen_sum(balance) AS total FROM users GROUP BY role HAVING yen_sum(balance) > $1
