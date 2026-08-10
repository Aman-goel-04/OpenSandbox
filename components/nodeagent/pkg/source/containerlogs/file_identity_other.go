//go:build !linux && !darwin

// Copyright 2026 Alibaba Group Holding Ltd.
// SPDX-License-Identifier: Apache-2.0

package containerlogs

import (
	"errors"
	"os"

	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
)

var errUnsupportedFileIdentity = api.Permanent(errors.New("container log fingerprinting is supported only on Linux and Darwin"))

func sourceFileIdentity(os.FileInfo) (uint64, uint64, error) {
	return 0, 0, errUnsupportedFileIdentity
}
