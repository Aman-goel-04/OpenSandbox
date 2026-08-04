//go:build linux || darwin

// Copyright 2026 Alibaba Group Holding Ltd.
// SPDX-License-Identifier: Apache-2.0

package containerlogs

import (
	"fmt"
	"os"
	"syscall"
)

func sourceFileIdentity(info os.FileInfo) (uint64, uint64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, permanent(fmt.Errorf("unsupported stat type %T for %q", info.Sys(), info.Name()))
	}
	return uint64(stat.Dev), uint64(stat.Ino), nil
}
