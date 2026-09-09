// Split mysqltest .test files into single statements for the spike driver.
// Output: statements separated by "\n;;\n"; each is prefixed with a comment
// "/* file:line [expect=ERR] */". Statements under --error are marked expect=.
import { readFileSync, readdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";

const dir = process.argv[2];
const out = process.argv[3];
const SQL_FIRST = /^(select|insert|update|delete|replace|create|drop|alter|set|show|explain|describe|desc|with|grant|revoke|flush|reset|start|begin|commit|rollback|savepoint|release|lock|unlock|analyze|optimize|repair|check|checksum|truncate|rename|load|call|do|handler|help|use|prepare|execute|deallocate|xa|install|uninstall|binlog|cache|kill|purge|change|stop|resume|import|clone|restart|shutdown|get|signal|resignal|table|values|declare|open|fetch|close|return|iterate|leave|loop|repeat|while|if|case)\b/i;
const CMD = /^(let|eval|connect|connection|disconnect|inc|dec|source|echo|exec|send|send_eval|reap|sleep|real_sleep|perl|delimiter|replace_regex|replace_column|replace_result|sorted_result|query_vertical|query_horizontal|error|enable_\w+|disable_\w+|vertical_results|horizontal_results|die|exit|skip|remove_file|remove_files_wildcard|write_file|append_file|copy_file|cat_file|list_files|list_files_write_file|list_files_append_file|move_file|mkdir|rmdir|chmod|change_user|character_set|start_timer|end_timer|result_format|shutdown_server|wait_for_slave_to_stop|sync_slave_with_master|sync_with_master|save_master_pos|file_exists|force-rmdir|force-cpdir|expr|assert|output|query|query_attributes|lowercase_result|reset_connection|ping|require|exec_in_background|partially_sorted_result|expr)\b/i;

let total = 0, kept = 0;
const chunks: string[] = [];
for (const f of readdirSync(dir).filter((x) => x.endsWith(".test")).sort()) {
  const lines = readFileSync(join(dir, f), "utf8").split("\n");
  let delim = ";";
  let expect: string | null = null;
  let buf: string[] = []; let bufLine = 0;
  let heredoc: string | null = null; // EOF terminator for write_file / perl / append_file
  for (let i = 0; i < lines.length; i++) {
    const raw = lines[i];
    if (heredoc !== null) { if (raw.trim() === heredoc) heredoc = null; continue; }
    if (buf.length === 0) {
      const t = raw.trim();
      if (t === "" || t.startsWith("#")) continue;
      const cmd = t.startsWith("--") ? t.slice(2).trim() : t;
      let m: RegExpExecArray | null;
      if ((m = /^(?:--\s*)?error\s+(.+?)\s*;?\s*$/i.exec(t))) { expect = m[1]; continue; }
      if ((m = /^(?:--\s*)?delimiter\s+(\S+?)\s*;?\s*$/i.exec(t))) { delim = m[1].replace(/;$/, "") || ";"; if (delim === "") delim = ";"; continue; }
      if ((m = /^(?:--\s*)?(?:perl|write_file|append_file)\b(?:.*\s(\w+))?\s*;?\s*$/i.exec(t)) && (t.startsWith("--") || /^(perl|write_file|append_file)\b/i.test(t))) { heredoc = /\b(perl|write_file|append_file)\s+\S+\s+(\w+)/i.exec(t)?.[2] ?? "EOF"; continue; }
      if (t.startsWith("--")) { continue; }                    // other commands
      if (CMD.test(cmd) || /^[{}]/.test(t) || /^(while|if)\s*\(/i.test(t)) { // command statement; may span to delim
        if (!raw.trimEnd().endsWith(delim)) { // skip until delimiter
          while (i + 1 < lines.length && !lines[i].trimEnd().endsWith(delim)) i++;
        }
        expect = null; continue;
      }
      if (!SQL_FIRST.test(t)) { expect = null; if (!raw.trimEnd().endsWith(delim)) { while (i + 1 < lines.length && !lines[i].trimEnd().endsWith(delim)) i++; } continue; }
      bufLine = i + 1;
    }
    buf.push(raw);
    const joined = buf.join("\n");
    if (joined.trimEnd().endsWith(delim)) {
      let stmt = joined.trimEnd(); stmt = stmt.slice(0, stmt.length - delim.length);
      total++;
      if (!/\$\w+/.test(stmt)) { kept++; chunks.push(`/* ${f}:${bufLine}${expect ? " expect=" + expect : ""} */\n${stmt}`); }
      buf = []; expect = null;
    }
  }
}
writeFileSync(out, chunks.join("\n;;\n") + "\n;;\n");
console.error(`statements=${total} kept=${kept} (dropped those with $vars)`);
