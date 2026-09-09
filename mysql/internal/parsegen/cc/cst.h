// The concrete syntax tree the generated actions build, and its wire format.
#pragma once
#include <cstddef>
#include <cstdint>
struct MY_SQL_PARSER_LTYPE;
class THD;

struct Node {
  uint16_t kind;      // index into kind_names (kinds.h): a rule, or a token for leaves
  bool leaf;
  const char *start;  // leaves: the token's span in the raw input
  const char *end;
  int n;
  Node **kids;
};

// mk builds a rule node over n children; L a leaf for the token at loc.
Node *mk(THD *thd, int kind, int n, ...);
Node *L(THD *thd, int kind, const MY_SQL_PARSER_LTYPE *loc);
