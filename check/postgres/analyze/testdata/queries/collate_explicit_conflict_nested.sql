SELECT id FROM users WHERE ((name COLLATE "C") || 'x') = (upper(alias) COLLATE "POSIX")
