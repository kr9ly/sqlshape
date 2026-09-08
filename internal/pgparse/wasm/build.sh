#!/usr/bin/env bash
# Builds pg_query_<major>.wasm from a libpg_query checkout with emscripten, and regenerates
# the Go node types (../pg_query.pb.go) from that checkout's pg_query.proto.
#
#   build.sh <libpg_query tag>            e.g. build.sh 17-6.2.2
#
# The tag is cloned (shallow) into $LIBPG_QUERY_CACHE (default ~/.cache/sqlshape/libpg_query)
# unless already present. Needs emcc, protoc and protoc-gen-go on PATH:
#   nix-shell -p emscripten protobuf protoc-gen-go --run "./build.sh 17-6.2.2"
#
# Why these flags:
#   -sSUPPORT_LONGJMP=emscripten  PostgreSQL reports a syntax error by longjmp (ereport ->
#                                 siglongjmp). Emscripten emulates it through invoke_* imports
#                                 that wazero's imports/emscripten package provides; wasm
#                                 exception handling would need a host that supports it.
#   -DDEBUG                       libpg_query redirects stderr through a pipe (dup2) unless
#                                 DEBUG is set; there is no pipe in wasm.
#   -sALLOW_MEMORY_GROWTH         a schema.sql or a regress corpus can be large.
#   -sERROR_ON_UNDEFINED_SYMBOLS=0  a few libc symbols the parser never reaches at runtime.
set -euo pipefail
tag=${1:?libpg_query tag, e.g. 17-6.2.2}
major=${tag%%-*}
here=$(cd "$(dirname "$0")" && pwd)
cache=${LIBPG_QUERY_CACHE:-$HOME/.cache/sqlshape/libpg_query}
src=$cache/$tag
if [ ! -d "$src" ]; then
	mkdir -p "$cache"
	git clone -q --depth 1 -b "$tag" https://github.com/pganalyze/libpg_query "$src"
fi
out=$here/pg_query_$major.wasm
srcs=$(ls "$src"/src/*.c "$src"/src/postgres/*.c)
srcs="$srcs $src/vendor/protobuf-c/protobuf-c.c $src/vendor/xxhash/xxhash.c $src/protobuf/pg_query.pb-c.c"
exports=_pg_query_init,_malloc,_free
exports=$exports,_shim_parse,_shim_parse_len,_shim_parse_data,_shim_parse_error,_shim_parse_cursor,_shim_parse_free
exports=$exports,_shim_deparse,_shim_deparse_query,_shim_deparse_error,_shim_deparse_free
exports=$exports,_shim_plpgsql,_shim_plpgsql_json,_shim_plpgsql_error,_shim_plpgsql_free
exports=$exports,_shim_split,_shim_split_n,_shim_split_location,_shim_split_len,_shim_split_error,_shim_split_free
emcc -O2 -std=gnu99 -fwrapv -fno-strict-aliasing \
	-I"$src" -I"$src/vendor" -I"$src/src/include" -I"$src/src/postgres/include" \
	-DDEBUG \
	-w \
	-sSUPPORT_LONGJMP=emscripten -sALLOW_MEMORY_GROWTH=1 -sSTACK_SIZE=4194304 \
	-sEXPORTED_FUNCTIONS=$exports \
	-sERROR_ON_UNDEFINED_SYMBOLS=0 --no-entry \
	-o "$out" $srcs "$here/shim.c"
ls -la "$out"
protoc --proto_path="$src/protobuf" --go_out="$here/.." \
	--go_opt=Mpg_query.proto=github.com/kr9ly/sqlshape/internal/pgparse --go_opt=paths=source_relative \
	"$src/protobuf/pg_query.proto"
gofmt -w "$here/../pg_query.pb.go"
