// Copyright 2026 Alibaba Group Holding Ltd.
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOSSAndFinalizeIdentityVectors(t *testing.T) {
	target, err := OSSTargetID("HTTPS://OSS-CN.EXAMPLE.COM:443/", "bucket-a", "/logs/", "prod-a")
	if err != nil {
		t.Fatal(err)
	}
	if want := "sha256:400cd6f60156b9e0c8165c3c228764974b80e0e84003abece9040c5ec8b28ec6"; target != want {
		t.Fatalf("target=%q want=%q", target, want)
	}
	if got, want := FinalizeID("container-logs/u123/sandbox", 2, "sha256:target"), "sha256:499a3cc01ca0b84d76764b8d7c60ec990fc42267278107ee9dc93a8052d58c96"; got != want {
		t.Fatalf("finalize=%q want=%q", got, want)
	}
}

func TestFileTargetIDStableBeforeAndAfterRootCreation(t *testing.T) {
	realRoot := t.TempDir()
	linkParent := filepath.Join(t.TempDir(), "data")
	if err := os.Symlink(realRoot, linkParent); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(linkParent, "logs", "nodeagent")
	before, err := FileTargetID(root, "cluster", "node")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	after, err := FileTargetID(root, "cluster", "node")
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("target changed after root creation: before=%s after=%s", before, after)
	}
}

func TestOSSTargetRejectsNonOrigin(t *testing.T) {
	for _, endpoint := range []string{"http://oss.example.com", "https://oss.example.com/path", "https://user@oss.example.com"} {
		if _, err := OSSTargetID(endpoint, "bucket", "logs", "prod"); err == nil {
			t.Fatalf("endpoint %q accepted", endpoint)
		}
	}
}
