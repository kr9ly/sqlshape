SELECT (o).*, (price).*, (row(1, 'x')).*, (save_order($1, $2)).* FROM orders o
