package migrate

import (
	"os"
	"testing"

	"github.com/kr9ly/sqlshape/mysqltest/v2"
)

// One mysqld per set of server settings serves the whole package (mysqltest.Main); a test
// gets it with the database dropped, not a server of its own.
func TestMain(m *testing.M) { os.Exit(mysqltest.Main(m)) }
