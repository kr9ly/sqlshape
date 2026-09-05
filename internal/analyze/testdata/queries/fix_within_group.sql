SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY total) AS med, percentile_disc(ARRAY[0.25, 0.75]) WITHIN GROUP (ORDER BY total) AS q,
       mode() WITHIN GROUP (ORDER BY status) AS m, rank(1000) WITHIN GROUP (ORDER BY total) AS hyp,
       percentile_cont(0.5) WITHIN GROUP (ORDER BY created_at - now()) AS medint
FROM orders
