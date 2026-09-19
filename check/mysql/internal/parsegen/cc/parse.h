// The parser's C entry points: what the wasm module exports and what the probe driver calls.
#pragma once
#include <stdint.h>
#ifdef __cplusplus
extern "C" {
#endif
// mysqlparse_init brings up the charset registry. Once per process/instance, before any parse.
int mysqlparse_init(void);

typedef struct mysqlparse_result mysqlparse_result;
// mysqlparse_parse parses one statement. sql_mode carries MySQL's own bits; only
// PIPES_AS_CONCAT, ANSI_QUOTES, IGNORE_SPACE, NO_BACKSLASH_ESCAPES and
// HIGH_NOT_PRECEDENCE reach the lexer.
mysqlparse_result *mysqlparse_parse(const char *sql, uint32_t len, uint32_t sql_mode);
// On error, the message and the byte offset of the token it points at; else NULL / 0.
const char *mysqlparse_error(const mysqlparse_result *r);
uint32_t mysqlparse_cursor(const mysqlparse_result *r);
// On success, the CST in preorder: per node u16 kind (bit 15 set on a leaf, bit 14 on a
// leaf that carries a value), then for a leaf u32 start, u32 end (byte offsets into sql)
// and, with bit 14, u32 length + bytes of the lexer's value (the identifier unquoted, the
// string literal unescaped, the charset of an introducer); for a rule u16 alternative index
// and u16 child count. Little-endian.
const uint8_t *mysqlparse_data(const mysqlparse_result *r);
uint32_t mysqlparse_len(const mysqlparse_result *r);
void mysqlparse_free(mysqlparse_result *r);
// mysqlparse_kind_name names a kind (see kinds.h); NULL when out of range.
const char *mysqlparse_kind_name(uint32_t kind);
uint32_t mysqlparse_kind_count(void);
#ifdef __cplusplus
}
#endif
