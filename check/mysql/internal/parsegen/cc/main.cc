// Probe driver: parse each statement of stdin (statements separated by a line ";;", each
// starting with a "/* file:line */" header) and print accept / reject per statement. Used
// to measure acceptance over mysql-test/t; see parsegen -corpus.
#include <cstdio>
#include <cstring>
#include <iostream>
#include <string>
#include "parse.h"

int main() {
  if (mysqlparse_init() != 0) { fprintf(stderr, "charset registry did not come up\n"); return 2; }
  std::string all((std::istreambuf_iterator<char>(std::cin)), {});
  size_t pos = 0;
  int ok = 0, ng = 0;
  while (pos < all.size()) {
    size_t e = all.find("\n;;\n", pos);
    std::string stmt = all.substr(pos, e == std::string::npos ? std::string::npos : e - pos);
    pos = e == std::string::npos ? all.size() : e + 4;
    if (stmt.find_first_not_of(" \t\r\n") == std::string::npos) continue;
    size_t nl = stmt.find('\n');
    std::string head = stmt.substr(0, nl);
    std::string body = nl == std::string::npos ? "" : stmt.substr(nl + 1);
    mysqlparse_result *r = mysqlparse_parse(stmt.data(), (uint32_t)stmt.size(), 0);
    for (char &ch : body) if (ch == '\n' || ch == '\t') ch = ' ';
    if (const char *err = mysqlparse_error(r)) {
      ng++;
      printf("FAIL\t%s\t%ld\t%.100s\n", head.c_str(), (long)mysqlparse_cursor(r) - (long)nl - 1, body.c_str());
    } else {
      ok++;
      printf("OK\t%s\t%u\t%.100s\n", head.c_str(), mysqlparse_len(r), body.c_str());
    }
    mysqlparse_free(r);
  }
  fprintf(stderr, "ok=%d fail=%d\n", ok, ng);
  return 0;
}
