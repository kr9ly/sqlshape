SELECT t.user_id, t.total, s.n FROM list_totals($1) t, LATERAL (SELECT count(*) AS n FROM orders WHERE user_id = t.user_id) s
