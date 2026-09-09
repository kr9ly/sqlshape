#include <cstdarg>
#include <cstdlib>
#include <cstdio>
#include "cst.h"
#include "sql/parse_location.h"
Node *mk(THD *, const char *rule, int n, ...) {
  Node *nd = (Node *)calloc(1, sizeof *nd); nd->kind = rule; nd->n = n;
  nd->kids = (Node **)calloc(n ? n : 1, sizeof(Node *));
  va_list ap; va_start(ap, n);
  for (int i = 0; i < n; i++) nd->kids[i] = va_arg(ap, Node *);
  va_end(ap);
  return nd;
}
Node *L(THD *, const char *tok, const MY_SQL_PARSER_LTYPE *loc) {
  Node *nd = (Node *)calloc(1, sizeof *nd); nd->kind = tok; nd->start = loc->raw.start; nd->end = loc->raw.end;
  return nd;
}
