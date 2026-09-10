SELECT t.a, t.b, t.n FROM unnest($1::int[], $2::text[]) WITH ORDINALITY AS t(a, b, n)
