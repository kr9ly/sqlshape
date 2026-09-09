// THD shim: the five things the lexer reads from the server's thread object.
#pragma once
#include <cstdint>
#include <cstdlib>
#include <cstring>
#include <vector>
#include "mysql/strings/m_ctype.h"
#include "lex_string.h"
struct Node;

using sql_mode_t = uint64_t;
inline constexpr sql_mode_t MODE_PIPES_AS_CONCAT = 2;
inline constexpr sql_mode_t MODE_ANSI_QUOTES = 4;
inline constexpr sql_mode_t MODE_IGNORE_SPACE = 8;
inline constexpr sql_mode_t MODE_NO_BACKSLASH_ESCAPES = 0x40000ULL * 4;  // MODE_ANSI * 2 * 2, as system_variables.h derives it
inline constexpr sql_mode_t MODE_HIGH_NOT_PRECEDENCE = 1ULL << 29;

inline bool operator==(const LEX_CSTRING &a, const LEX_CSTRING &b) { return a.length == b.length && (a.length == 0 || memcmp(a.str, b.str, a.length) == 0); }
class Lex_input_stream;
class Parser_state;
struct sql_digest_state;

struct System_variables {
  sql_mode_t sql_mode = 0;
  const CHARSET_INFO *default_collation_for_utf8mb4 = nullptr;
  const CHARSET_INFO *character_set_client = nullptr;
};

class THD {
 public:
  System_variables variables;
  std::vector<Node *> cst_nodes;      // every node built during this parse, freed together
  const char *parse_error = nullptr;  // bison's message, set by my_sql_parser_error
  Parser_state *m_parser_state = nullptr;
  const CHARSET_INFO *m_charset = nullptr;
  const CHARSET_INFO *charset() const { return m_charset; }
  bool is_error() const { return false; }
  struct Da { bool has_sql_condition(int) const { return false; } } m_da;
  const Da *get_parser_da() const { return &m_da; }
  void *alloc(size_t n) { return malloc(n); }
  char *strmake(const char *s, size_t n) { char *p = (char *)malloc(n + 1); memcpy(p, s, n); p[n] = 0; return p; }
};

struct Sql_condition { enum enum_severity_level { SL_NOTE, SL_WARNING, SL_ERROR }; };
enum { ER_WARN_NO_SPACE_VERSION_COMMENT = 1, ER_WARN_DEPRECATED_SYNTAX_NO_REPLACEMENT, ER_WARN_DEPRECATED_NESTED_COMMENT_SYNTAX, ER_CAPACITY_EXCEEDED };
inline const char *ER_THD(const THD *, int) { return ""; }
inline void push_warning(THD *, Sql_condition::enum_severity_level, unsigned, const char *) {}
inline void push_deprecated_warn(THD *, const char *, const char *) {}
inline void push_deprecated_warn_no_replacement(THD *, const char *) {}
inline void warn_on_deprecated_charset(THD *, const CHARSET_INFO *, const char *) {}
inline void warn_on_deprecated_collation(THD *, const CHARSET_INFO *) {}
extern CHARSET_INFO my_charset_utf8mb4_0900_ai_ci;  // referenced by the lexer; the spike never has it as a real collation
#include "mysql/strings/collations.h"
