// Lexer body, cut from sql/sql_lex.cc (find_keyword .. lex_one_token) with the
// server-side plumbing (digest, utf8 body, optimizer hints) shimmed.
#include "sql/sql_lex.h"
#include <cstdlib>
#include <cstring>
#include "m_string.h"
#include "mysql_version.h"
#include "my_dbug.h"
#include "sql/lex_symbol.h"
#include "sql/sql_lex_hash.h"
#include "sql/sql_yacc.h"

static int lex_one_token(Lexer_yystype *yylval, THD *thd);

