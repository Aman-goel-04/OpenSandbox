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

package routescope

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

type scopeFixture struct {
	KeyID        string `json:"key_id"`
	SecretBase64 string `json:"secret_base64"`
	Namespace    string `json:"namespace"`
	SandboxID    string `json:"sandbox_id"`
	Port         int    `json:"port"`
	Token        string `json:"token"`
}

func loadFixture(t *testing.T) (scopeFixture, []byte) {
	t.Helper()
	data, err := os.ReadFile("../../../../specs/fixtures/ingress-route-scope-v1.json")
	require.NoError(t, err)
	var fixture scopeFixture
	require.NoError(t, json.Unmarshal(data, &fixture))
	secret, err := base64.StdEncoding.DecodeString(fixture.SecretBase64)
	require.NoError(t, err)
	return fixture, secret
}

func TestVerifyCrossLanguageVector(t *testing.T) {
	fixture, secret := loadFixture(t)
	verifier := &Verifier{Keys: map[string][]byte{fixture.KeyID: secret}}
	scope, err := verifier.Verify(fixture.Token)
	require.NoError(t, err)
	require.Equal(t, Scope{Namespace: fixture.Namespace, SandboxID: fixture.SandboxID, Port: fixture.Port}, scope)
}

func TestVerifyRejectsTamperedNamespace(t *testing.T) {
	verifier := &Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}}
	_, err := verifier.Verify("f1.dGVuYW50LWI.c2FuZGJveC0xMjM.44772.k.uo11HjECmnSuCCRF3v-1AQ")
	require.ErrorIs(t, err, ErrUnauthorized)
}

func TestVerifyRejectsNonCanonicalBase64URL(t *testing.T) {
	verifier := &Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}}
	for _, token := range []string{
		"f1.dGVuYW50LWE.c2FuZGJveC0xMjM.44772.k.uo11HjECmnSuCCRF3v-1AR",
		"f1.dGVuYW50LWF.c2FuZGJveC0xMjM.44772.k.uo11HjECmnSuCCRF3v-1AQ",
	} {
		_, err := verifier.Verify(token)
		require.ErrorIs(t, err, ErrInvalidScope)
	}
}

func TestVerifyRejectsNonCanonicalPort(t *testing.T) {
	verifier := &Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}}
	_, err := verifier.Verify("f1.dGVuYW50LWE.c2FuZGJveC0xMjM.+44772.k.uo11HjECmnSuCCRF3v-1AQ")
	require.ErrorIs(t, err, ErrInvalidScope)
}

func TestVerifyRejectsMalformedScope(t *testing.T) {
	verifier := &Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}}
	_, err := verifier.Verify("f1.bad")
	require.True(t, errors.Is(err, ErrInvalidScope))
}
