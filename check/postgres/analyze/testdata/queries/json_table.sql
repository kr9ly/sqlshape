SELECT o.id, jt.n, jt.item_id, jt.name, jt.has_price, jt.tag
FROM orders o, JSON_TABLE(o.meta, '$.items[*]' COLUMNS (n FOR ORDINALITY, item_id int PATH '$.id', name text PATH '$.name' DEFAULT 'n/a' ON EMPTY, has_price bool EXISTS PATH '$.price', NESTED PATH '$.tags[*]' COLUMNS (tag text PATH '$'))) AS jt
WHERE jt.item_id = $1
