// Spike driver: parse each statement of stdin (separated by a line containing only ';;')
// and print accept / reject with the CST size.
#include <cstdio>
#include <cstring>
#include <string>
#include <iostream>
#include "sql/sql_lex.h"
#include "sql/sql_yacc.h"
#include "mysql/strings/m_ctype.h"

#include "mysql/strings/collations.h"
struct Loader : MY_CHARSET_LOADER {
  void reporter(enum loglevel, unsigned, ...) override {}
  void *read_file(const char *, size_t *) override { return nullptr; }
};

static int count(Node *n) { if (!n) return 0; int c = 1; for (int i = 0; i < n->n; i++) c += count(n->kids[i]); return c; }
static const char *last_err;
void my_sql_parser_error(MY_SQL_PARSER_LTYPE *, THD *, Node **, const char *msg) { last_err = msg; }

int main() {
  mysql::collation::initialize(nullptr, new Loader);  // the registry owns the loader; it also builds the lexer state maps
  const CHARSET_INFO *cs = mysql::collation::find_primary("utf8mb4");
  if (!cs || !cs->state_maps) { fprintf(stderr, "charset registry did not come up\n"); return 2; }
  std::string all((std::istreambuf_iterator<char>(std::cin)), {});
  size_t pos = 0; int ok = 0, ng = 0;
  while (pos < all.size()) {
    size_t e = all.find("\n;;\n", pos);
    std::string stmt = all.substr(pos, e == std::string::npos ? std::string::npos : e - pos);
    pos = e == std::string::npos ? all.size() : e + 4;
    if (stmt.find_first_not_of(" \t\r\n") == std::string::npos) continue;
    THD thd; thd.m_charset = cs;
    thd.variables.default_collation_for_utf8mb4 = cs;
    thd.variables.character_set_client = cs;
    Parser_state ps; thd.m_parser_state = &ps;
    std::string buf = stmt; buf.push_back(0);
    ps.init(&thd, buf.data(), stmt.size()); ps.m_lip.stmt_prepare_mode = true;
    Node *out = nullptr; last_err = nullptr;
    int rc = my_sql_parser_parse(&thd, &out);
    size_t nl = stmt.find('\n'); std::string head = stmt.substr(0, nl); std::string body = nl == std::string::npos ? "" : stmt.substr(nl + 1);
    for (char &ch : body) if (ch == '\n' || ch == '\t') ch = ' ';
    if (rc == 0) { ok++; printf("OK\t%s\t%d\t%.100s\n", head.c_str(), count(out), body.c_str()); }
    else { ng++; long at = (long)(ps.m_lip.get_tok_start() - buf.data()) - (long)nl - 1; printf("FAIL\t%s\t%ld\t%.100s\n", head.c_str(), at, body.c_str()); }
  }
  fprintf(stderr, "ok=%d fail=%d\n", ok, ng);
  return 0;
}
