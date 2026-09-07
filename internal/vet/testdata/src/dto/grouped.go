package dto

import (
	"errors"
)

var _ = errors.New

// Grouped already has a parenthesized import: a fix that needs a new one (pgtype, for
// numeric) adds it inside the existing block.
type Grouped struct{}
