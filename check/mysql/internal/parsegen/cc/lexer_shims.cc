// --- shims for what the server does with the token stream besides parsing
void Lex_input_stream::body_utf8_start(THD *, const char *) {}
void Lex_input_stream::body_utf8_append(const char *, const char *) {}
void Lex_input_stream::body_utf8_append(const char *) {}
void Lex_input_stream::body_utf8_append_literal(THD *, const LEX_STRING *, const CHARSET_INFO *, const char *) {}
void Lex_input_stream::add_digest_token(uint, Lexer_yystype *) {}
void Lex_input_stream::reduce_digest_token(uint, uint) {}

// Optimizer hints: skip the /*+ ... */ comment instead of parsing it.
static bool consume_optimizer_hints(Lex_input_stream *lip) {
  const my_lex_states *state_map = lip->query_charset->state_maps->main_map;
  int whitespace = 0;
  uchar c = lip->yyPeek();
  size_t newlines = 0;
  for (; state_map[c] == MY_LEX_SKIP; whitespace++, c = lip->yyPeekn(whitespace)) {
    if (c == '\n') newlines++;
  }
  if (lip->yyPeekn(whitespace) == '/' && lip->yyPeekn(whitespace + 1) == '*' && lip->yyPeekn(whitespace + 2) == '+') {
    lip->yylineno += newlines;
    lip->yySkipn(whitespace + 3);
    for (;;) {
      if (lip->eof()) return true;  // unterminated hint comment
      c = lip->yyGet();
      if (c == '\n') lip->yylineno++;
      if (c == '*' && lip->yyPeek() == '/') { lip->yySkip(); break; }
    }
    lip->yylval->optimizer_hints = nullptr;
  }
  return false;
}

