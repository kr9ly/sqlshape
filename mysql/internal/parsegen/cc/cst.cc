#include <cstdarg>
#include <cstdlib>
#include "cst.h"
#include "sql/parse_location.h"
#include "sql/sql_lex.h"

Node *mk(THD *thd, int kind, int n, ...) {
  Node *nd = (Node *)calloc(1, sizeof *nd);
  nd->kind = (uint16_t)kind;
  nd->n = n;
  nd->kids = (Node **)calloc(n ? n : 1, sizeof(Node *));
  va_list ap;
  va_start(ap, n);
  for (int i = 0; i < n; i++) nd->kids[i] = va_arg(ap, Node *);
  va_end(ap);
  thd->cst_nodes.push_back(nd);
  return nd;
}

Node *L(THD *thd, int kind, const MY_SQL_PARSER_LTYPE *loc) {
  Node *nd = (Node *)calloc(1, sizeof *nd);
  nd->kind = (uint16_t)kind;
  nd->leaf = true;
  nd->start = loc->raw.start;
  nd->end = loc->raw.end;
  thd->cst_nodes.push_back(nd);
  return nd;
}
