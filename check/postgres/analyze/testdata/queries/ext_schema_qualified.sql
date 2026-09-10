SELECT public.crypt(name, public.gen_salt('md5')), pg_catalog.lower(name) FROM users
