module github.com/kr9ly/sqlshape/mysql

go 1.26.1

// The root module is required by version, like cmd/sqlshape does: bump it to the root tag
// that goes out with each release (internal/dialect is what this module needs of it).
require github.com/kr9ly/sqlshape v1.2.0
