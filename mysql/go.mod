module github.com/kr9ly/sqlshape/mysql

go 1.26.1

// The root module is required by version; scripts/release.sh keeps it at the release's
// version (the three modules release in lockstep). internal/dialect is what is needed of it.
require github.com/kr9ly/sqlshape v1.2.0
