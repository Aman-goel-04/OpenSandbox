// Copyright 2026 Alibaba Group Holding Ltd.
// SPDX-License-Identifier: Apache-2.0

// Package resourcecheck validates host reserves that cannot be expressed by
// the Helm schema.
package resourcecheck

import "github.com/alibaba/opensandbox/nodeagent/pkg/config"

func Validate(cfg config.Config) error { return validateHost(cfg) }
