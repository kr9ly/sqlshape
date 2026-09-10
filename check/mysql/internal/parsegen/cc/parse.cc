#include "parse.h"
#include <cstdlib>
#include <cstring>
#include <string>
#include <vector>
#include "cst.h"
#include "kinds.h"
#include "mysql/strings/collations.h"
#include "mysql/strings/m_ctype.h"
#include "sql/sql_lex.h"
#include "sql/sql_yacc.h"

struct Loader : MY_CHARSET_LOADER {
  void reporter(enum loglevel, unsigned, ...) override {}
  void *read_file(const char *, size_t *) override { return nullptr; }
};

static const CHARSET_INFO *utf8mb4;

int mysqlparse_init(void) {
  if (utf8mb4) return 0;
  mysql::collation::initialize(nullptr, new Loader);  // the registry also builds the lexer's state maps
  utf8mb4 = mysql::collation::find_primary("utf8mb4");
  return utf8mb4 && utf8mb4->state_maps ? 0 : 1;
}

struct mysqlparse_result {
  std::string error;
  uint32_t cursor = 0;
  std::string data;
};

// The parser reports through this; the message is bison's, the position is the token the
// lexer last started.
void my_sql_parser_error(MY_SQL_PARSER_LTYPE *, THD *thd, Node **, const char *msg) {
  thd->parse_error = msg;
}

static void put16(std::string &s, uint32_t v) { s.push_back((char)(v & 0xff)); s.push_back((char)(v >> 8)); }
static void put32(std::string &s, uint32_t v) { for (int i = 0; i < 4; i++) s.push_back((char)((v >> (8 * i)) & 0xff)); }

// Spans are clamped to the text: END_OF_INPUT is the NUL the lexer reads past the end.
static void serialize(std::string &s, const Node *n, const char *base, uint32_t len) {
  if (n->leaf) {
    uint32_t start = n->start ? (uint32_t)(n->start - base) : len;
    uint32_t end = n->end ? (uint32_t)(n->end - base) : len;
    if (start > len) start = len;
    if (end > len) end = len;
    put16(s, n->kind | 0x8000u | (n->hasval ? 0x4000u : 0));
    put32(s, start);
    put32(s, end);
    if (n->hasval) {
      put32(s, n->vlen);
      s.append(n->val, n->vlen);
    }
    return;
  }
  put16(s, n->kind);
  put16(s, n->alt);
  put16(s, (uint32_t)n->n);
  for (int i = 0; i < n->n; i++) serialize(s, n->kids[i], base, len);
}

mysqlparse_result *mysqlparse_parse(const char *sql, uint32_t len, uint32_t sql_mode) {
  auto *r = new mysqlparse_result;
  if (!utf8mb4) { r->error = "mysqlparse_init was not called"; return r; }
  // The lexer wants a NUL after the text and may patch version comments in place.
  std::string buf(sql, len);
  buf.push_back(0);
  THD thd;
  thd.m_charset = utf8mb4;
  thd.variables.sql_mode = sql_mode;
  thd.variables.default_collation_for_utf8mb4 = utf8mb4;
  thd.variables.character_set_client = utf8mb4;
  Parser_state ps;
  thd.m_parser_state = &ps;
  ps.init(&thd, buf.data(), len);
  ps.m_lip.stmt_prepare_mode = true;  // '?' is a placeholder
  Node *out = nullptr;
  int rc = my_sql_parser_parse(&thd, &out);
  if (rc != 0 || out == nullptr) {
    r->error = thd.parse_error ? thd.parse_error : "syntax error";
    const char *tok = ps.m_lip.get_tok_start();
    r->cursor = tok && tok >= buf.data() ? (uint32_t)(tok - buf.data()) : 0;
  } else {
    r->data.reserve(thd.cst_nodes.size() * 6);
    serialize(r->data, out, buf.data(), len);
  }
  for (Node *n : thd.cst_nodes) { free(n->kids); free(n); }
  return r;
}

const char *mysqlparse_error(const mysqlparse_result *r) { return r->error.empty() ? nullptr : r->error.c_str(); }
uint32_t mysqlparse_cursor(const mysqlparse_result *r) { return r->cursor; }
const uint8_t *mysqlparse_data(const mysqlparse_result *r) { return (const uint8_t *)r->data.data(); }
uint32_t mysqlparse_len(const mysqlparse_result *r) { return (uint32_t)r->data.size(); }
void mysqlparse_free(mysqlparse_result *r) { delete r; }
const char *mysqlparse_kind_name(uint32_t kind) { return kind < KIND_COUNT ? kind_names[kind] : nullptr; }
uint32_t mysqlparse_kind_count(void) { return KIND_COUNT; }
