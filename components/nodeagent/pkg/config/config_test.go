// Copyright 2026 Alibaba Group Holding Ltd.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

func TestLoadFileConfig(t *testing.T) {
	t.Setenv("NODE_NAME", "node-1")
	t.Setenv("NODEAGENT_CLUSTER_ID", "prod-a")
	t.Setenv("NODEAGENT_SINKS", "file")
	t.Setenv("NODEAGENT_LOG_ROOT", t.TempDir())
	t.Setenv("NODEAGENT_STATE_DIR", t.TempDir())
	t.Setenv("NODEAGENT_FILE_PATH", t.TempDir())

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.NodeName != "node-1" || cfg.ClusterID != "prod-a" || cfg.Sink != SinkFile {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestLoadRejectsInvalidIdentityAndBudget(t *testing.T) {
	t.Setenv("NODE_NAME", "node-1")
	t.Setenv("NODEAGENT_CLUSTER_ID", "INVALID")
	t.Setenv("NODEAGENT_LOG_ROOT", t.TempDir())
	t.Setenv("NODEAGENT_STATE_DIR", t.TempDir())
	t.Setenv("NODEAGENT_MEMORY_BUDGET_BYTES", "10")
	t.Setenv("NODEAGENT_PER_SANDBOX_QUEUE_BYTES", "11")

	if _, err := Load(); err == nil {
		t.Fatal("Load() expected an error")
	}
}

func TestValidateRejectsConflictingServerAddresses(t *testing.T) {
	tests := []struct {
		name       string
		serverAddr string
		pprofAddr  string
	}{
		{name: "empty wildcard", serverAddr: ":8080", pprofAddr: "127.0.0.1:8080"},
		{name: "IPv4 wildcard", serverAddr: "0.0.0.0:8080", pprofAddr: "127.0.0.1:8080"},
		{name: "IPv6 wildcard", serverAddr: "[::]:8080", pprofAddr: "[::1]:8080"},
		{name: "localhost alias", serverAddr: "localhost:8080", pprofAddr: "127.0.0.1:8080"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.ServerAddr = tt.serverAddr
			cfg.PprofAddr = tt.pprofAddr
			if err := errorsContaining(cfg.validate(), "must not conflict"); err == "" {
				t.Fatalf("validate() accepted conflicting addresses %q and %q", tt.serverAddr, tt.pprofAddr)
			}
		})
	}
}

func TestValidateRejectsInvalidListenPort(t *testing.T) {
	for _, address := range []string{":not-a-port", ":http", "localhost:", ":0", ":70000"} {
		cfg := validConfig()
		cfg.ServerAddr = address
		if err := errorsContaining(cfg.validate(), "NODEAGENT_SERVER_ADDR"); err == "" {
			t.Fatalf("validate() accepted invalid listen address %q", address)
		}
	}
}

func TestValidateAbsolutePathRejectsSegmentsNotNames(t *testing.T) {
	if err := validateAbsolutePath("/var/lib/nodeagent..data"); err != nil {
		t.Fatalf("validateAbsolutePath() rejected a safe name: %v", err)
	}
	for _, value := range []string{"/", "/var/lib/../etc", "/var/*/data"} {
		if err := validateAbsolutePath(value); err == nil {
			t.Fatalf("validateAbsolutePath(%q) unexpectedly succeeded", value)
		}
	}
}

func TestValidateOSSEndpointRejectsCredentials(t *testing.T) {
	if err := validateOSSEndpoint("https://oss.example.com"); err != nil {
		t.Fatalf("validateOSSEndpoint() rejected a valid origin: %v", err)
	}
	for _, endpoint := range []string{
		"https://user@oss.example.com",
		"https://user:password@oss.example.com",
	} {
		if err := validateOSSEndpoint(endpoint); err == nil {
			t.Fatalf("validateOSSEndpoint(%q) unexpectedly succeeded", endpoint)
		}
	}
}

func validConfig() Config {
	return Config{
		NodeName:             "node-1",
		ClusterID:            "prod-a",
		Source:               "container-logs",
		Sink:                 SinkFile,
		LogRoot:              "/var/log/pods",
		StateDir:             "/var/lib/opensandbox/nodeagent",
		MemoryBudgetBytes:    1024,
		PerSandboxQueueBytes: 1024,
		MaxLineBytes:         1,
		DropPolicy:           "block",
		BatchMaxItems:        1,
		FileMaxFiles:         1,
		ServerAddr:           ":8080",
		ContainerNames:       []string{"sandbox"},
	}
}

func errorsContaining(errs []error, substring string) string {
	for _, err := range errs {
		if strings.Contains(err.Error(), substring) {
			return err.Error()
		}
	}
	return ""
}
