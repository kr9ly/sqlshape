#pragma once
#include <cstddef>
struct MY_SQL_PARSER_LTYPE;
class THD;
struct Node {
  const char *kind;   // rule name or token name
  const char *start;  // source span (raw buffer), leaves only
  const char *end;
  int n;
  Node **kids;
};
Node *mk(THD *thd, const char *rule, int n, ...);
Node *L(THD *thd, const char *tok, const MY_SQL_PARSER_LTYPE *loc);
