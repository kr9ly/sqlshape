module github.com/kr9ly/sqlshape/check/mysql/v2

go 1.26.1

// The root module is required by version; scripts/release.sh keeps it at the release's
// version (the three modules release in lockstep). internal/dialect is what is needed of it.

require (
	github.com/go-sql-driver/mysql v1.10.1
	github.com/kr9ly/sqlshape/v2 v2.0.0-rc.1
	github.com/tetratelabs/wazero v1.12.0
)

require (
	filippo.io/edwards25519 v1.2.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
)
