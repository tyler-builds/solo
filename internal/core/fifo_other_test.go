//go:build !unix

package core

import "errors"

func mkfifo(string) error { return errors.New("no named pipes here") }
