#!/usr/bin/env bash
# Spike build: MySQL's own grammar and lexer, cut out of the server source, as a CST parser.
#   ./build.sh          native binary ./mysqlparse
#   ./build.sh wasm     also wasm/mysqlparse.{js,wasm} via emcc
# Needs: node >= 22, bison 3.8, g++ (and emscripten for wasm), MySQL source in $MYSQL_SRC.
set -euo pipefail
cd "$(dirname "$0")"
M=${MYSQL_SRC:-$HOME/.cache/sqlshape/mysql-server}
INC="-I. -Ishim -Igen -I$M/include -I$M -I$M/strings"
mkdir -p gen/sql wasm

# 1. grammar: strip the semantic actions, keep the token numbering
node --experimental-strip-types strip.ts "$M/sql/sql_yacc.yy" grammar.y 2> >(grep -v ExperimentalWarning >&2 || true)
bison -Wnone --defines=parser.h -o parser.cc grammar.y          # %expect 59 is checked by bison itself

# 2. keyword hash: the server's generator, run against our parser.h token numbers
bison -Wnone --defines=gen/sql/sql_hints.yy.h -o /dev/null "$M/sql/sql_hints.yy"
g++ -std=c++20 -DNDEBUG -w $INC -o gen/gen_lex_hash "$M/sql/gen_lex_hash.cc"
./gen/gen_lex_hash > gen/sql/lex_hash.h

# 3. lexer: sql_lex.h's Lex_input_stream and sql_lex.cc's token functions, between our head/tail/shim files
{
  cat shim/sql/sql_lex_head.h
  sed -n '/^enum enum_comment_state {/,/^class Lex_input_stream {/p' "$M/sql/sql_lex.h" | sed '$d'
  sed -n '/^class Lex_input_stream {/,/^};/p' "$M/sql/sql_lex.h"
  cat shim/sql/sql_lex_tail.h
} > shim/sql/sql_lex.h
{
  cat lexer_head.cc
  echo "// --- sql_lex.cc: Lex_input_stream::init / reset"
  sed -n '/^bool Lex_input_stream::init(THD \*thd/,/^}/p' "$M/sql/sql_lex.cc"
  sed -n '/^void Lex_input_stream::reset(const char \*buffer/,/^}/p' "$M/sql/sql_lex.cc"
  cat lexer_shims.cc
  echo "// --- sql_lex.cc: find_keyword .. lex_one_token"
  sed -n '/^static int find_keyword(Lex_input_stream \*lip/,/^void trim_whitespace(/p' "$M/sql/sql_lex.cc" | sed '$d' \
    | sed '/^[A-Za-z_]*_parser_state::[A-Za-z_]*_parser_state()$/,/^    : Parser_state(.*) {}$/d'
} > lexer.cc

SRCS="main.cc cst.cc lexer.cc parser.cc link_stubs.cc $M/sql/sql_lex_hash.cc
      $M/strings/ctype-utf8.cc $M/strings/ctype-mb.cc $M/strings/ctype-simple.cc $M/strings/ctype-bin.cc
      $M/strings/sql_chars.cc $M/strings/my_uctype.cc $M/strings/my_strtoll10.cc $M/strings/dtoa.cc"
g++ -std=c++20 -DNDEBUG -O2 -w $INC -o mysqlparse $SRCS
echo "built ./mysqlparse"
if [ "${1:-}" = wasm ]; then
  em++ -std=c++20 -DNDEBUG -O2 -w $INC -o wasm/mysqlparse.js $SRCS
  echo "built wasm/mysqlparse.{js,wasm}"
fi
