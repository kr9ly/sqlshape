class Parser_state {
 public:
  Parser_state() : m_lip(~0U) {}
  bool init(THD *thd, const char *buff, size_t length) { return m_lip.init(thd, buff, length); }
  void add_comment() { m_comment = true; }
  Lex_input_stream m_lip;
  bool m_comment = false;
};
