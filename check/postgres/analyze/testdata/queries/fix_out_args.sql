SELECT e.key, e.value, t.key AS k2, t.value AS v2, r.a FROM orders o, jsonb_each(o.meta) e, json_each_text(o.meta::json) t, jsonb_to_record(o.meta) AS r(a int)
