// The concrete syntax tree the generated actions build, and its wire format.
#pragma once
#include <cstddef>
#include <cstdint>
struct MY_SQL_PARSER_LTYPE;
class THD;

struct Node {
  uint16_t kind;      // index into kind_names (kinds.h): a rule, or a token for leaves
  uint16_t alt;       // rules: which alternative of the rule was reduced (0-based, grammar order)
  bool leaf;
  const char *start;  // leaves: the token's span in the raw input
  const char *end;
  const char *val;    // leaves with a lexer value (identifiers unquoted, literals unescaped): the value
  uint32_t vlen;
  bool hasval;
  int n;
  Node **kids;
};
struct MYSQL_LEX_STRING;
struct CHARSET_INFO;

// mk builds a rule node for alternative alt over n children; L a leaf for the token at loc.
// LS is a leaf with the lexer's string value, LC one whose value is a charset's name.
Node *mk(THD *thd, int kind, int alt, int n, ...);
Node *L(THD *thd, int kind, const MY_SQL_PARSER_LTYPE *loc);
Node *LS(THD *thd, int kind, const MY_SQL_PARSER_LTYPE *loc, const MYSQL_LEX_STRING *s);
Node *LC(THD *thd, int kind, const MY_SQL_PARSER_LTYPE *loc, const CHARSET_INFO *cs);
