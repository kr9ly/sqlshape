#include <cstdarg>
#include <cstdlib>
#include <cstring>
#include "mysql/strings/m_ctype.h"
#include "cst.h"
#include "sql/parse_location.h"
#include "sql/sql_lex.h"

Node *mk(THD *thd, int kind, int alt, int n, ...) {
  Node *nd = (Node *)calloc(1, sizeof *nd);
  nd->kind = (uint16_t)kind;
  nd->alt = (uint16_t)alt;
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

Node *LS(THD *thd, int kind, const MY_SQL_PARSER_LTYPE *loc, const MYSQL_LEX_STRING *s) {
  Node *nd = L(thd, kind, loc);
  if (s->str) {
    nd->val = s->str;
    nd->vlen = (uint32_t)s->length;
    nd->hasval = true;
  }
  return nd;
}

Node *LC(THD *thd, int kind, const MY_SQL_PARSER_LTYPE *loc, const CHARSET_INFO *cs) {
  Node *nd = L(thd, kind, loc);
  if (cs) {
    nd->val = cs->csname;
    nd->vlen = (uint32_t)strlen(cs->csname);
    nd->hasval = true;
  }
  return nd;
}
