//go:build !unix

package preflight

import "errors"

func fileLimit() (soft, hard uint64, err error) { return 0, 0, errors.New("not supported") }
