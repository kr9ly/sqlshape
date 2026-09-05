SELECT x, save_order(price, ARRAY[]::order_items[]) AS saved FROM user_ids() AS x, orders
