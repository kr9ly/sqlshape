SELECT coalesce(note, 'none') AS note, coalesce($1, total) AS c2, CASE status WHEN 'paid' THEN 1 ELSE 0 END AS paid, CASE WHEN total > $2 THEN 'big' END AS size FROM orders
