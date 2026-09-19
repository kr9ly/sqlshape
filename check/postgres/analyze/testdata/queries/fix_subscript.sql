SELECT meta['a'] AS ja, meta['a']['b'] AS jab, meta['a'][0] AS ja0, matrix[1][2] AS m12, matrix[1] AS m1, matrix[1:2] AS msl, matrix[1:2][1] AS msl2, tags[1] AS t1, tags[$1] AS tp
FROM orders o, users u
