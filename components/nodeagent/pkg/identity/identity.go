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

package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func OSSTargetID(endpoint, bucket, prefix, clusterID string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(u.Scheme, "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return "", errors.New("OSS endpoint must be an HTTPS origin")
	}
	if bucket == "" || strings.Trim(prefix, "/") == "" || clusterID == "" {
		return "", errors.New("OSS target identity fields are required")
	}
	host := strings.ToLower(u.Hostname())
	if port := u.Port(); port != "" && port != "443" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	normalized := "https://" + host
	return digest("opensandbox-nodeagent-target-v1\x00", "oss", normalized, bucket, strings.Trim(prefix, "/"), clusterID), nil
}

func FileTargetID(root, clusterID, nodeName string) (string, error) {
	canonical, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return "", err
	}
	canonical, err = resolveExistingAncestor(canonical)
	if err != nil {
		return "", err
	}
	if canonical == string(filepath.Separator) || clusterID == "" || nodeName == "" {
		return "", errors.New("file target identity fields are invalid")
	}
	return digest("opensandbox-nodeagent-target-v1\x00", "file", canonical, clusterID, nodeName), nil
}

func resolveExistingAncestor(path string) (string, error) {
	current := path
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for index := len(suffix) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, suffix[index])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func StdoutTargetID(clusterID, nodeName string) string {
	return digest("opensandbox-nodeagent-target-v1\x00", "stdout", clusterID, nodeName)
}

func FinalizeID(streamRef string, revision uint64, targetID string) string {
	return digest("opensandbox-nodeagent-finalize-v1\x00", streamRef, strconv.FormatUint(revision, 10), targetID)
}

func digest(domain string, parts ...string) string {
	h := sha256.New()
	_, _ = h.Write([]byte(domain))
	for _, part := range parts {
		_, _ = fmt.Fprintf(h, "%d:%s", len([]byte(part)), part)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}
