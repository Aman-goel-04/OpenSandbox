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

package proxy

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/alibaba/opensandbox/ingress/pkg/routescope"
	"github.com/alibaba/opensandbox/ingress/pkg/sandbox"
	"github.com/alibaba/opensandbox/ingress/pkg/signature"
	"github.com/alibaba/opensandbox/ingress/pkg/telemetry"
)

type Mode string

const (
	ModeHeader Mode = "header"
	ModeURI    Mode = "uri"
)

func (p *Proxy) getSandboxHostDefinition(r *http.Request) (*sandboxHost, int, error) {
	start := time.Now()
	host, status, err := p.doGetSandboxHostDefinition(r)
	durationMs := float64(time.Since(start)) / float64(time.Millisecond)

	result := "success"
	if err != nil {
		result = routingResultFromErr(err)
	}
	telemetry.RecordRouting(result, durationMs)
	return host, status, err
}

func routingResultFromErr(err error) string {
	if errors.Is(err, sandbox.ErrSandboxNotFound) {
		return "not_found"
	}
	if errors.Is(err, sandbox.ErrSandboxNotReady) {
		return "not_ready"
	}
	return "error"
}

func (p *Proxy) doGetSandboxHostDefinition(r *http.Request) (*sandboxHost, int, error) {
	var pr parsedRoute
	var err error

	switch p.mode {
	case ModeHeader:
		targetHost := p.parseTargetHostByHeader(r)
		if targetHost == "" {
			return nil, http.StatusBadRequest, fmt.Errorf("missing header '%s' or 'Host'", SandboxIngress)
		}
		if routescope.IsToken(targetHost) {
			pr, err = p.parseFleetsScope(targetHost, "")
		} else {
			pr, err = parseHostRoute(targetHost)
		}
	case ModeURI:
		if r.URL == nil || r.URL.Path == "" {
			return nil, http.StatusBadRequest, errors.New("missing URI path")
		}
		trimmed := strings.TrimPrefix(r.URL.Path, "/")
		first, rest, found := strings.Cut(trimmed, "/")
		if routescope.IsToken(first) {
			requestURI := "/"
			if found && rest != "" {
				requestURI += rest
			}
			pr, err = p.parseFleetsScope(first, requestURI)
			if err == nil {
				pr.requestRawPath = escapedPathSuffix(r.URL.EscapedPath(), 1)
			}
		} else {
			pr, err = parseURIRoute(r.URL.Path)
			if err != nil {
				break
			} else if pr.uriParsedAsOSEP {
				pr.requestRawPath = escapedPathSuffix(r.URL.EscapedPath(), 4)
			} else {
				pr.requestRawPath = escapedPathSuffix(r.URL.EscapedPath(), 2)
			}
		}
	default:
		return nil, http.StatusBadRequest, fmt.Errorf("unknown ingress mode: %s", p.mode)
	}

	if err != nil || pr.sandboxID == "" || pr.port == 0 {
		if err == nil {
			err = errors.New("missing sandbox ID or port")
		}
		return nil, ingressRouteErrHTTPStatus(err), fmt.Errorf("invalid ingress route: %w", err)
	}
	if _, required := p.sandboxProvider.(sandbox.AuthenticatedRouteProvider); required && pr.namespace == "" {
		err = fmt.Errorf("%w: fleets provider requires an authenticated route scope", routescope.ErrUnauthorized)
		return nil, ingressRouteErrHTTPStatus(err), err
	}

	endpoint, err := p.sandboxProvider.ResolveEndpoint(r.Context(), sandbox.EndpointTarget{
		Namespace: pr.namespace,
		SandboxID: pr.sandboxID,
		Port:      pr.port,
	})
	if err != nil {
		return nil, providerErrHTTPStatus(err), err
	}

	need := endpoint.AccessVerificationRequired()

	if p.mode == ModeURI && !need && pr.uriParsedAsOSEP {
		pr, err = parseURILegacy(r.URL.Path)
		if err != nil {
			return nil, ingressRouteErrHTTPStatus(err), err
		}
		pr.requestRawPath = escapedPathSuffix(r.URL.EscapedPath(), 2)
	}

	present, accessTok := signature.SecureAccessHeaderInfo(r)
	if err := signature.CheckIngressSecureAccess(signature.IngressAccessInput{
		Secure:                    need,
		ExpectedAccessToken:       endpoint.SecureAccessToken,
		SecureAccessHeaderPresent: present,
		RequestedAccessToken:      accessTok,
		ExpiresB36:                pr.expiresB36,
		Signature:                 pr.signature,
		SandboxID:                 pr.sandboxID,
		Port:                      pr.port,
		Verifier:                  p.secure,
	}); err != nil {
		return nil, ingressRouteErrHTTPStatus(err), err
	}

	return &sandboxHost{
		namespace:      pr.namespace,
		ingressKey:     pr.sandboxID,
		port:           pr.port,
		endpoint:       endpoint.Endpoint,
		info:           endpoint,
		requestURI:     pr.requestURI,
		requestRawPath: pr.requestRawPath,
	}, 0, nil
}

func escapedPathSuffix(path string, prefixSegments int) string {
	trimmed := strings.TrimPrefix(path, "/")
	for range prefixSegments {
		_, rest, found := strings.Cut(trimmed, "/")
		if !found {
			return "/"
		}
		trimmed = rest
	}
	if trimmed == "" {
		return "/"
	}
	return "/" + trimmed
}

func (p *Proxy) parseFleetsScope(token, requestURI string) (parsedRoute, error) {
	if p.scope == nil {
		return parsedRoute{}, fmt.Errorf("%w: verifier is not configured", routescope.ErrUnauthorized)
	}
	scope, err := p.scope.Verify(token)
	if err != nil {
		return parsedRoute{}, err
	}
	return parsedRoute{
		namespace:  scope.Namespace,
		sandboxID:  scope.SandboxID,
		port:       scope.Port,
		requestURI: requestURI,
	}, nil
}

func (p *Proxy) parseTargetHostByHeader(r *http.Request) string {
	targetHost := r.Header.Get(SandboxIngress)
	if targetHost != "" {
		return targetHost
	}
	deprecatedTargetHost := r.Header.Get(DeprecatedSandboxIngress)
	if deprecatedTargetHost != "" {
		return deprecatedTargetHost
	}

	return r.Host
}

type sandboxHost struct {
	namespace      string
	ingressKey     string
	port           int
	endpoint       string
	info           *sandbox.EndpointInfo
	requestURI     string
	requestRawPath string
}
