#!/usr/bin/env bash
# Spike build: MySQL's own grammar and lexer, cut out of the server source, as a CST parser.
#   ./build.sh          native binary ./mysqlparse
#   ./build.sh wasm     also wasm/mysqlparse.{js,wasm} via emcc
# Needs: node >= 22, bison 3.8, g++ (and emscripten for wasm), MySQL source in $MYSQL_SRC.
# The spike measures two things: the grammar with its actions stripped, and the server's own
# lexer + charset registry compiled behind a THD shim.
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

# 4. charset registry: every compiled collation the server has, plus the two generated han tables
g++ -std=c++20 -DNDEBUG -O1 -w $INC -o gen/uca9dump "$M/strings/uca9-dump.cc"
mkdir -p gen/strings
[ -f gen/strings/uca900_ja_tbls.cc ] || ./gen/uca9dump ja --in_file="$M/strings/lang_data/ja_hans.txt" --out_file=gen/strings/uca900_ja_tbls.cc
[ -f gen/strings/uca900_zh_tbls.cc ] || ./gen/uca9dump zh --in_file="$M/strings/lang_data/zh_hans.txt" --out_file=gen/strings/uca900_zh_tbls.cc
LIB="$M/sql/sql_lex_hash.cc gen/strings/uca900_ja_tbls.cc gen/strings/uca900_zh_tbls.cc
     $(ls "$M"/strings/ctype*.cc "$M"/strings/collations*.cc)
     $M/strings/xml.cc $M/strings/int2str.cc $M/strings/my_strchr.cc $M/strings/str_alloc.cc $M/strings/sql_chars.cc
     $M/strings/my_uctype.cc $M/strings/my_strtoll10.cc $M/strings/dtoa.cc"
OURS="main.cc cst.cc lexer.cc parser.cc"

# 5. build (library objects are cached under gen/obj and gen/wobj; rm -rf gen to rebuild them)
compile() { # <compiler> <objdir> <output>
  local cxx=$1 objdir=$2 out=$3; mkdir -p "$objdir"
  for f in $LIB; do o="$objdir/$(basename "$f" .cc).o"; [ -f "$o" ] || $cxx -std=c++20 -DNDEBUG -O2 -w $INC -c -o "$o" "$f" & done; wait
  $cxx -std=c++20 -DNDEBUG -O2 -w $INC -o "$out" $OURS "$objdir"/*.o
}
compile g++ gen/obj mysqlparse
echo "built ./mysqlparse"
if [ "${1:-}" = wasm ]; then
  compile em++ gen/wobj wasm/mysqlparse.js
  echo "built wasm/mysqlparse.{js,wasm}"
fi
