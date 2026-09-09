// Strip semantic actions from MySQL's sql_yacc.yy and emit a grammar whose every
// alternative builds a generic CST node. Throwaway spike.
import { readFileSync, writeFileSync } from "node:fs";

const src = readFileSync(process.argv[2], "utf8");
const out = process.argv[3];

// --- split sections -------------------------------------------------------
const pct = [...src.matchAll(/^%%\s*$/gm)].map((m) => m.index!);
if (pct.length < 1) throw new Error("no %%");
const decl = src.slice(0, pct[0]);
const rules = src.slice(pct[0] + 2, pct[1] ?? src.length);

// --- token names (terminals) ----------------------------------------------
const tokenNames = new Set<string>();
for (const m of decl.matchAll(/^%token\s*(?:<[^>]*>)?\s+([A-Za-z_][A-Za-z0-9_]*)/gm)) tokenNames.add(m[1]);
// --- declarations ---------------------------------------------------------
let d = decl.replace(/%\{[\s\S]*?%\}/, "");            // drop C prologue
d = d.replace(/^%parse-param.*$/gm, "");
d = d.replace(/^%lex-param.*$/gm, "");
d = d.replace(/^%define api\.pure.*$/gm, "");
d = d.replace(/^%type\s*<[^>]*>/gm, "%type ");          // typeless nonterminals (fixed below)
d = d.replace(/^(%token)\s*<[^>]*>/gm, "$1");
d = d.replace(/^(%left|%right|%nonassoc|%precedence)\s*<[^>]*>/gm, "$1");
// %type without a tag is not legal; drop the %type lines entirely (api.value.type covers it)
d = d.replace(/^%type\b[^\n]*(\n[ \t]+[^\n%][^\n]*)*/gm, "");

const prologue = `%{
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include "cst.h"
#define YYINITDEPTH 100
#define YYMAXDEPTH 3200
%}
%code requires {
#include "cst.h"
#include "sql/lexer_yystype.h"
#include "sql/parse_location.h"
union CST_STYPE { Node *node; Lexer_yystype lexer; };
}
%define api.pure full
%define api.value.type { CST_STYPE }
%define api.location.type { MY_SQL_PARSER_LTYPE }
%locations
%parse-param { class THD *YYTHD } { Node **out }
%lex-param { class THD *YYTHD }
%code provides {
int my_sql_parser_lex(CST_STYPE *v, MY_SQL_PARSER_LTYPE *l, class THD *thd);
void my_sql_parser_error(MY_SQL_PARSER_LTYPE *l, class THD *thd, Node **out, const char *msg);
}
`;

// --- rules: tokenizer -----------------------------------------------------
type Tok = { k: "id" | "lit" | "colon" | "bar" | "semi" | "act" | "prec" | "empty" | "ws"; s: string };
function lex(s: string): Tok[] {
  const t: Tok[] = [];
  let i = 0;
  const n = s.length;
  while (i < n) {
    const c = s[i];
    if (c === "/" && s[i + 1] === "*") { const j = s.indexOf("*/", i + 2); t.push({ k: "ws", s: s.slice(i, j + 2) }); i = j + 2; continue; }
    if (c === "/" && s[i + 1] === "/") { const j = s.indexOf("\n", i); t.push({ k: "ws", s: s.slice(i, j) }); i = j; continue; }
    if (/\s/.test(c)) { let j = i; while (j < n && /\s/.test(s[j])) j++; t.push({ k: "ws", s: s.slice(i, j) }); i = j; continue; }
    if (c === "{") { const j = matchBrace(s, i); t.push({ k: "act", s: s.slice(i, j) }); i = j; continue; }
    if (c === "'" || c === '"') { let j = i + 1; while (s[j] !== c) { if (s[j] === "\\") j++; j++; } t.push({ k: "lit", s: s.slice(i, j + 1) }); i = j + 1; continue; }
    if (c === ":") { t.push({ k: "colon", s: c }); i++; continue; }
    if (c === "|") { t.push({ k: "bar", s: c }); i++; continue; }
    if (c === ";") { t.push({ k: "semi", s: c }); i++; continue; }
    if (c === "%") { const m = /^%(prec|empty)\b/.exec(s.slice(i)); if (!m) throw new Error("unknown % at " + line(s, i)); t.push({ k: m[1] as any, s: m[0] }); i += m[0].length; continue; }
    const m = /^[A-Za-z_.][A-Za-z0-9_.]*/.exec(s.slice(i));
    if (!m) throw new Error(`unexpected ${JSON.stringify(s.slice(i, i + 20))} at line ${line(s, i)}`);
    t.push({ k: "id", s: m[0] }); i += m[0].length;
  }
  return t;
}
function line(s: string, i: number) { return s.slice(0, i).split("\n").length; }
function matchBrace(s: string, i: number): number {
  let depth = 0;
  for (let j = i; j < s.length; j++) {
    const c = s[j];
    if (c === "/" && s[j + 1] === "*") { j = s.indexOf("*/", j + 2) + 1; continue; }
    if (c === "/" && s[j + 1] === "/") { j = s.indexOf("\n", j); continue; }
    if (c === '"' || c === "'") { let k = j + 1; while (s[k] !== c) { if (s[k] === "\\") k++; k++; } j = k; continue; }
    if (c === "{") depth++;
    else if (c === "}") { depth--; if (depth === 0) return j + 1; }
  }
  throw new Error("unbalanced brace at line " + line(s, i));
}

const startSym = /^%start\s+(\S+)/m.exec(decl)![1];
// --- rules: rebuild -------------------------------------------------------
const toks = lex(rules).filter((t) => t.k !== "ws");
let o = "";
let i = 0;
let nRules = 0, nAlts = 0, nMid = 0;
const ruleNames: string[] = [];
while (i < toks.length) {
  const lhs = toks[i]; if (lhs.k !== "id" || toks[i + 1]?.k !== "colon") throw new Error("expected rule head, got " + JSON.stringify(toks.slice(i, i + 3)));
  i += 2; nRules++; ruleNames.push(lhs.s);
  o += `${lhs.s}:\n`;
  let first = true;
  for (;;) {
    // one alternative
    const syms: string[] = []; let prec = ""; let mids = 0;
    for (;;) {
      const t = toks[i];
      if (!t || t.k === "bar" || t.k === "semi") break;
      if (t.k === "id" && toks[i + 1]?.k === "colon") break; // next rule head, no ';'
      i++;
      if (t.k === "id" || t.k === "lit") syms.push(t.s);
      else if (t.k === "empty") { /* nothing */ }
      else if (t.k === "prec") { prec = ` %prec ${toks[i].s}`; i++; }
      else if (t.k === "act") {
        // action: final if next is bar/semi, else mid-rule
        const nx = toks[i];
        if (nx && nx.k !== "bar" && nx.k !== "semi") { syms.push("{}"); mids++; }
      }
    }
    nAlts++; nMid += mids;
    const real = syms.filter((x) => x !== "{}");
    const isTerm = (x: string) => x.startsWith("'") || x.startsWith('"') || tokenNames.has(x);
    const args = syms.map((x, k) => (x === "{}" ? null : isTerm(x) ? `L(YYTHD, ${JSON.stringify(x)}, &@${k + 1})` : `$${k + 1}.node`)).filter(Boolean).join(", ");
    const body = real.length === 0 ? "%empty" : syms.join(" ");
    o += `${first ? "  " : "| "}${body}${prec} { $$.node = mk(YYTHD, "${lhs.s}", ${real.length}${real.length ? ", " + args : ""});${lhs.s === startSym ? " *out = $$.node;" : ""} }\n`;
    first = false;
    if (toks[i]?.k === "bar") { i++; continue; }
    if (toks[i]?.k === "semi") { i++; break; }
    break; // implicit end (next rule head)
  }
  o += ";\n\n";
}
writeFileSync(out, prologue + d + "\n%%\n\n" + o + "\n%%\n");
console.error(`rules=${nRules} alternatives=${nAlts} mid-rule-actions=${nMid}`);
