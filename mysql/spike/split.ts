// Split mysqltest .test files into single statements for the spike driver.
// Output: statements separated by "\n;;\n"; each is prefixed with a comment
// "/* file:line [expect=ERR] */". Statements under --error are marked expect=.
// Follows mysqltest's reading: a command runs to the current delimiter found outside
// quotes; whatever follows the delimiter on the same line starts the next command.
import { readFileSync, readdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";

const dir = process.argv[2];
const out = process.argv[3];
const SQL_FIRST = /^(select|insert|update|delete|replace|create|drop|alter|set|show|explain|describe|desc|with|grant|revoke|flush|reset|start|begin|commit|rollback|savepoint|release|lock|unlock|analyze|optimize|repair|check|checksum|truncate|rename|load|call|do|handler|help|use|prepare|execute|deallocate|xa|install|uninstall|binlog|cache|kill|purge|change|stop|resume|import|clone|restart|shutdown|get|signal|resignal|table|values|declare|open|fetch|close|return|iterate|leave|loop|repeat|while|if|case)\b/i;
const HEREDOC = /^(?:--\s*)?(perl|write_file|append_file)\b/i;

let total = 0, kept = 0;
const chunks: string[] = [];

// Find the delimiter in `line` outside quotes and comments; returns index or -1.
function findDelim(line: string, delim: string, st: { q: string | null; blockComment: boolean }): number {
  for (let i = 0; i < line.length; i++) {
    const c = line[i];
    if (st.blockComment) { if (c === "*" && line[i + 1] === "/") { st.blockComment = false; i++; } continue; }
    if (st.q) { if (c === "\\" && st.q !== "`") { i++; continue; } if (c === st.q) st.q = null; continue; }
    if (c === "'" || c === '"' || c === "`") { st.q = c; continue; }
    if (c === "/" && line[i + 1] === "*") { st.blockComment = true; i++; continue; }
    if (c === "#" ) return -1;                                   // rest of line is a comment
    if (c === "-" && line[i + 1] === "-" && (i === 0 || /\s/.test(line[i - 1])) && (line[i + 2] === undefined || /\s/.test(line[i + 2]))) return -1;
    if (line.startsWith(delim, i)) return i;
  }
  return -1;
}

for (const f of readdirSync(dir).filter((x) => x.endsWith(".test")).sort()) {
  const lines = readFileSync(join(dir, f), "latin1").split("\n");
  let delim = ";";
  let expect: string | null = null;
  let heredoc: string | null = null;
  // current command
  let buf: string[] = []; let bufLine = 0; let isSql = false;
  let st = { q: null as string | null, blockComment: false };
  let i = 0;
  let pending: string | null = null;  // remainder of a line after a delimiter
  while (i < lines.length || pending !== null) {
    let raw: string; const lineNo = i + 1;
    if (pending !== null) { raw = pending; pending = null; } else { raw = lines[i++]; }
    if (heredoc !== null) { if (raw.trim() === heredoc) heredoc = null; continue; }
    if (buf.length === 0) {
      const t = raw.trim();
      if (t === "" || t.startsWith("#")) continue;
      let m: RegExpExecArray | null;
      if ((m = /^(?:--\s*)?error\s+(.+?)\s*;?\s*$/i.exec(t))) { expect = m[1]; continue; }
      if ((m = /^(?:--\s*)?delimiter\s+(.*)$/i.exec(t))) {
        let d = m[1].trim(); if (d.endsWith(delim) && d.length > delim.length) d = d.slice(0, -delim.length); if (d.endsWith(";") && d.length > 1 && delim === ";") d = d.slice(0, -1);
        delim = d || ";"; continue;
      }
      if (HEREDOC.test(t)) { heredoc = /\b(perl|write_file|append_file)\b(?:\s+\S+)?\s+(\w+)\s*;?\s*$/i.exec(t)?.[2] ?? "EOF"; continue; }
      if (t.startsWith("--")) { expect = expect; continue; }         // other one-line commands keep --error for the next statement
      isSql = SQL_FIRST.test(t) && !/^(if|while)\s*\(/i.test(t);
      bufLine = lineNo; st = { q: null, blockComment: false };
    }
    const at = findDelim(raw, delim, st);
    if (at < 0) { buf.push(raw); continue; }
    buf.push(raw.slice(0, at));
    const rest = raw.slice(at + delim.length); if (rest.trim() !== "") pending = rest;
    const stmt = buf.join("\n").trim(); buf = [];
    if (isSql) {
      total++;
      if (!/\$\w+/.test(stmt)) { kept++; chunks.push(`/* ${f}:${bufLine}${expect ? " expect=" + expect : ""} */\n${stmt}`); }
      expect = null;
    } else if (!/^\{?\s*$/.test(stmt)) { expect = null; }  // a non-SQL command consumed the --error (blocks `{` / `}` do not)
  }
}
writeFileSync(out, chunks.join("\n;;\n") + "\n;;\n", "latin1");
console.error(`statements=${total} kept=${kept} (dropped those with $vars)`);
