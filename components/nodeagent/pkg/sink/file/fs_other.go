//go:build !linux

// Copyright 2026 Alibaba Group Holding Ltd.
// SPDX-License-Identifier: Apache-2.0

package file

import (
	"errors"
	"fmt"
	"os"
)

var errUnsupportedPlatform = permanent(errors.New("durable file sink is supported only on Linux"))

func openNoFollow(path string) (*os.File, error) {
	return nil, errUnsupportedPlatform
}
func openNoFollowRead(path string) (*os.File, error) {
	return nil, errUnsupportedPlatform
}
func openNoFollowExisting(path string) (*os.File, error) {
	return nil, errUnsupportedPlatform
}
func createNoFollowExclusive(path string) (*os.File, error) {
	return nil, errUnsupportedPlatform
}
func mkdirAllNoFollow(string, os.FileMode) error {
	return errUnsupportedPlatform
}
func syncData(f *os.File) error            { return f.Sync() }
func renameNoReplace(string, string) error { return errUnsupportedPlatform }
func syncDir(string) error                 { return errUnsupportedPlatform }
func fileIdentity(os.FileInfo) (uint64, uint64, error) {
	return 0, 0, errUnsupportedPlatform
}
func classifyPathError(operation, path string, err error) error {
	wrapped := fmt.Errorf("%s %q: %w", operation, path, err)
	if errors.Is(err, os.ErrPermission) {
		return permanent(wrapped)
	}
	return wrapped
}
