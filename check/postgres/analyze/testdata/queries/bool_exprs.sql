SELECT id, status = 'paid' AS paid, NOT (total > 10) AS small, total > 10 AND note IS NULL AS both, $1 AND true AS p FROM orders WHERE $2
