// Package ptytest opens pseudo-terminals for tests of terminal handling.
package ptytest

import (
	"errors"
	"os"
)

func closeOnError(err error, primary *os.File) error { return errors.Join(err, primary.Close()) }
