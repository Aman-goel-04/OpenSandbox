// Copyright 2026 Alibaba Group Holding Ltd.
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"testing"

	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
	"github.com/alibaba/opensandbox/nodeagent/pkg/config"
)

func TestRegisteredSinkProvidesTargetIdentity(t *testing.T) {
	const name = "test-sink-target"
	RegisterSink(
		name,
		func(cfg config.Config) (string, error) { return "target:" + cfg.ClusterID, nil },
		func(Dependencies) (api.Sink, error) { return nil, nil },
	)

	got, err := TargetID(name, config.Config{ClusterID: "prod-a"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "target:prod-a" {
		t.Fatalf("TargetID() = %q", got)
	}
}
