SELECT id FROM orders WHERE status IN ('paid', 'shipped') AND id IN ($1, $2) AND user_id = ANY($3) AND note LIKE $4
