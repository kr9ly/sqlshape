// Symbols the charset files reference but the lexer never reaches.
#include <cstdlib>
#include <vector>
#include "mysql/strings/m_ctype.h"
int (*my_string_stack_guard)(int) = nullptr;
MY_CHARSET_LOADER::~MY_CHARSET_LOADER() = default;
void *MY_CHARSET_LOADER::once_alloc(size_t n) { return malloc(n); }
struct MY_CONTRACTION;
uint16_t my_uca_contraction2_weight(const std::vector<MY_CONTRACTION> *, size_t, size_t) { abort(); }
