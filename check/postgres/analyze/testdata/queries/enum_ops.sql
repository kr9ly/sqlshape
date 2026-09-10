SELECT status, status::text AS st, status < 'shipped' AS early, enum_range(NULL::order_status) AS all_statuses FROM orders WHERE status = $1 ORDER BY status
