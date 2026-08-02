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

package imagecommitter

import (
	"testing"

	"github.com/containerd/containerd/images"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestCommitMediaTypesUsesOCIDiffForDockerManifest(t *testing.T) {
	mediaTypes := commitMediaTypes(images.MediaTypeDockerSchema2Manifest)
	if mediaTypes.Manifest != images.MediaTypeDockerSchema2Manifest {
		t.Fatalf("manifest media type = %q", mediaTypes.Manifest)
	}
	if mediaTypes.Config != images.MediaTypeDockerSchema2Config {
		t.Fatalf("config media type = %q", mediaTypes.Config)
	}
	if mediaTypes.Layer != images.MediaTypeDockerSchema2LayerGzip {
		t.Fatalf("manifest layer media type = %q", mediaTypes.Layer)
	}
	if mediaTypes.Diff != ocispec.MediaTypeImageLayerGzip {
		t.Fatalf("diff service media type = %q, want OCI gzip", mediaTypes.Diff)
	}
}

func TestCommitMediaTypesUsesOCIForOCIManifest(t *testing.T) {
	mediaTypes := commitMediaTypes(ocispec.MediaTypeImageManifest)
	if mediaTypes.Manifest != ocispec.MediaTypeImageManifest ||
		mediaTypes.Config != ocispec.MediaTypeImageConfig ||
		mediaTypes.Layer != ocispec.MediaTypeImageLayerGzip ||
		mediaTypes.Diff != ocispec.MediaTypeImageLayerGzip {
		t.Fatalf("unexpected OCI media types: %#v", mediaTypes)
	}
}
