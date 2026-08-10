// Copyright 2026 Alibaba Group Holding Ltd.
// SPDX-License-Identifier: Apache-2.0

package containerlogs

import (
	"io/fs"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
)

type unsupportedFileInfo struct{}

func (unsupportedFileInfo) Name() string       { return "unsupported.log" }
func (unsupportedFileInfo) Size() int64        { return 0 }
func (unsupportedFileInfo) Mode() fs.FileMode  { return 0 }
func (unsupportedFileInfo) ModTime() time.Time { return time.Time{} }
func (unsupportedFileInfo) IsDir() bool        { return false }
func (unsupportedFileInfo) Sys() any           { return struct{}{} }

func TestSourceFileIdentityClassifiesUnsupportedStatType(t *testing.T) {
	_, _, err := sourceFileIdentity(unsupportedFileInfo{})
	if err == nil || api.IsRetryableError(err) {
		t.Fatalf("error=%v retryable=%v, want permanent error", err, api.IsRetryableError(err))
	}
}
