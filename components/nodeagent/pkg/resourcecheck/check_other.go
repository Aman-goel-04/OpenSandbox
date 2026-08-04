//go:build !linux

// Copyright 2026 Alibaba Group Holding Ltd.
// SPDX-License-Identifier: Apache-2.0

package resourcecheck

import (
	"errors"

	"github.com/alibaba/opensandbox/nodeagent/pkg/config"
)

func validateHost(config.Config) error {
	return errors.New("host resource validation is supported only on Linux")
}
