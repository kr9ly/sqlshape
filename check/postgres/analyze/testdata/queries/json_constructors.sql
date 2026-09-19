SELECT JSON_OBJECT('id': id, 'note': note ABSENT ON NULL) AS o, JSON_OBJECT('a': 1 RETURNING jsonb) AS ob, JSON_ARRAY(id, note, $1) AS ar, JSON_ARRAY(SELECT id FROM orders) AS aq,
       JSON(note) AS j, JSON_SCALAR(id) AS js, JSON_SERIALIZE(meta) AS ser, JSON_SERIALIZE(meta RETURNING bytea) AS serb
FROM orders
