SELECT crypt($1, gen_salt('bf')) AS hash, digest(name, 'sha256') AS d, encode(gen_random_bytes(16), 'hex') AS tok,
       uuid_generate_v4() AS u
FROM users WHERE crypt($2, alias) = alias
