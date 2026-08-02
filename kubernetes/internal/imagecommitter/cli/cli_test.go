// Copyright 2026 Alibaba Group Holding Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/imagecommitter"
)

func TestParseCommitOperation(t *testing.T) {
	t.Setenv("SOURCE_POD_UID", "pod-uid")
	operation, request, _, err := parseOperation([]string{
		"pod-1",
		"default",
		"main:registry.example.com:5000/snapshots/main:snap",
		"sidecar:registry.example.com/snapshots/sidecar:snap",
	})
	if err != nil {
		t.Fatalf("parseOperation failed: %v", err)
	}
	if operation != "commit" {
		t.Fatalf("operation = %q, want commit", operation)
	}
	if request.PodName != "pod-1" || request.Namespace != "default" || request.PodUID != "pod-uid" {
		t.Fatalf("unexpected request identity: %#v", request)
	}
	if len(request.Containers) != 2 {
		t.Fatalf("container count = %d, want 2", len(request.Containers))
	}
	if got := request.Containers[0].Target; got != "registry.example.com:5000/snapshots/main:snap" {
		t.Fatalf("target = %q", got)
	}
}

func TestParseUnpauseOperation(t *testing.T) {
	operation, _, request, err := parseOperation([]string{"unpause", "pod-1", "default", "main", "sidecar"})
	if err != nil {
		t.Fatalf("parseOperation failed: %v", err)
	}
	if operation != "unpause" {
		t.Fatalf("operation = %q, want unpause", operation)
	}
	if len(request.ContainerNames) != 2 || request.ContainerNames[1] != "sidecar" {
		t.Fatalf("unexpected container names: %v", request.ContainerNames)
	}
}

func TestParseOperationRejectsInvalidInput(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"pod", "namespace"},
		{"pod", "namespace", "invalid"},
		{"unpause", "pod", "namespace"},
	} {
		if _, _, _, err := parseOperation(args); err == nil {
			t.Fatalf("parseOperation(%v) unexpectedly succeeded", args)
		}
	}
}

func TestWriteResult(t *testing.T) {
	terminationMessagePath := filepath.Join(t.TempDir(), "termination.log")

	want := imagecommitter.Result{Containers: []imagecommitter.ContainerResult{
		{Name: "main", Image: "registry.example.com/main:snap", Digest: "sha256:main"},
		{Name: "sidecar", Image: "registry.example.com/sidecar:snap", Digest: "sha256:sidecar"},
	}}
	if err := writeResult(terminationMessagePath, want); err != nil {
		t.Fatalf("writeResult failed: %v", err)
	}
	data, err := os.ReadFile(terminationMessagePath)
	if err != nil {
		t.Fatalf("read termination result: %v", err)
	}
	var got imagecommitter.Result
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode termination result: %v", err)
	}
	if len(got.Containers) != 2 || got.Containers[0] != want.Containers[0] || got.Containers[1] != want.Containers[1] {
		t.Fatalf("unexpected result: %#v", got)
	}
}

func TestShouldUseInsecureRegistry(t *testing.T) {
	t.Run("explicit false overrides heuristic", func(t *testing.T) {
		t.Setenv("SNAPSHOT_REGISTRY_INSECURE", "false")
		if shouldUseInsecureRegistry("registry.local/snapshot:test", os.Stderr) {
			t.Fatal("explicit false should disable insecure transport")
		}
	})
	t.Run("private host heuristic", func(t *testing.T) {
		t.Setenv("SNAPSHOT_REGISTRY_INSECURE", "")
		if !shouldUseInsecureRegistry("10.0.0.2:5000/snapshot:test", os.Stderr) {
			t.Fatal("private registry should use compatibility heuristic")
		}
	})
}
