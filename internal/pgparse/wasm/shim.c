// Pointer-returning wrappers around the libpg_query C API.
//
// libpg_query returns its result structs by value; a wasm host cannot receive a struct
// return, so each entry point here allocates the result and hands back a pointer, and the
// fields are read through accessors. The host frees the result with the matching *_free.
#include <stdlib.h>
#include <string.h>
#include "pg_query.h"

// parse: SQL text -> protobuf-encoded ParseResult
PgQueryProtobufParseResult* shim_parse(const char* input) {
	PgQueryProtobufParseResult* r = malloc(sizeof(PgQueryProtobufParseResult));
	*r = pg_query_parse_protobuf(input);
	return r;
}
unsigned int shim_parse_len(PgQueryProtobufParseResult* r) { return r->parse_tree.len; }
char* shim_parse_data(PgQueryProtobufParseResult* r) { return r->parse_tree.data; }
char* shim_parse_error(PgQueryProtobufParseResult* r) { return r->error ? r->error->message : NULL; }
int shim_parse_cursor(PgQueryProtobufParseResult* r) { return r->error ? r->error->cursorpos : 0; }
void shim_parse_free(PgQueryProtobufParseResult* r) { pg_query_free_protobuf_parse_result(*r); free(r); }

// parse_json: SQL text -> JSON ParseResult (field names, not numbers: readable across
// libpg_query versions, whose protobuf field numbers are not stable)
PgQueryParseResult* shim_parse_json(const char* input) {
	PgQueryParseResult* r = malloc(sizeof(PgQueryParseResult));
	*r = pg_query_parse(input);
	return r;
}
char* shim_parse_json_tree(PgQueryParseResult* r) { return r->parse_tree; }
char* shim_parse_json_error(PgQueryParseResult* r) { return r->error ? r->error->message : NULL; }
int shim_parse_json_cursor(PgQueryParseResult* r) { return r->error ? r->error->cursorpos : 0; }
void shim_parse_json_free(PgQueryParseResult* r) { pg_query_free_parse_result(*r); free(r); }

// deparse: protobuf-encoded ParseResult -> SQL text
PgQueryDeparseResult* shim_deparse(char* data, unsigned int len) {
	PgQueryProtobuf p; p.data = data; p.len = len;
	PgQueryDeparseResult* r = malloc(sizeof(PgQueryDeparseResult));
	*r = pg_query_deparse_protobuf(p);
	return r;
}
char* shim_deparse_query(PgQueryDeparseResult* r) { return r->query; }
char* shim_deparse_error(PgQueryDeparseResult* r) { return r->error ? r->error->message : NULL; }
void shim_deparse_free(PgQueryDeparseResult* r) { pg_query_free_deparse_result(*r); free(r); }

// plpgsql: CREATE FUNCTION text -> JSON of the PL/pgSQL parse trees
PgQueryPlpgsqlParseResult* shim_plpgsql(const char* input) {
	PgQueryPlpgsqlParseResult* r = malloc(sizeof(PgQueryPlpgsqlParseResult));
	*r = pg_query_parse_plpgsql(input);
	return r;
}
char* shim_plpgsql_json(PgQueryPlpgsqlParseResult* r) { return r->plpgsql_funcs; }
char* shim_plpgsql_error(PgQueryPlpgsqlParseResult* r) { return r->error ? r->error->message : NULL; }
void shim_plpgsql_free(PgQueryPlpgsqlParseResult* r) { pg_query_free_plpgsql_parse_result(*r); free(r); }

// split: SQL text -> statement byte spans, by the scanner alone (no grammar)
PgQuerySplitResult* shim_split(const char* input) {
	PgQuerySplitResult* r = malloc(sizeof(PgQuerySplitResult));
	*r = pg_query_split_with_scanner(input);
	return r;
}
int shim_split_n(PgQuerySplitResult* r) { return r->n_stmts; }
int shim_split_location(PgQuerySplitResult* r, int i) { return r->stmts[i]->stmt_location; }
int shim_split_len(PgQuerySplitResult* r, int i) { return r->stmts[i]->stmt_len; }
char* shim_split_error(PgQuerySplitResult* r) { return r->error ? r->error->message : NULL; }
void shim_split_free(PgQuerySplitResult* r) { pg_query_free_split_result(*r); free(r); }
