// Copyright 2026 Alibaba Group Holding Ltd.
// SPDX-License-Identifier: Apache-2.0

package objectlayout

import "testing"

func TestObjectFamilyLayout(t *testing.T) {
	family := FamilyPrefix("logs", "prod", "ns", "sb", "uid")
	if family != "logs/prod/ns/sb/uid" {
		t.Fatalf("family=%q", family)
	}
	if got := DataKey(family, "sandbox", 0); got != "logs/prod/ns/sb/uid/sandbox.log" {
		t.Fatalf("generation zero=%q", got)
	}
	if got := DataKey(family, "sandbox", 2); got != "logs/prod/ns/sb/uid/sandbox.2.log" {
		t.Fatalf("generation two=%q", got)
	}
	if got := MarkerPrefix(family, "sandbox"); got != "logs/prod/ns/sb/uid/sandbox.finalized." {
		t.Fatalf("marker prefix=%q", got)
	}
	if got := MarkerKey(family, "sandbox", 3); got != "logs/prod/ns/sb/uid/sandbox.finalized.3.json" {
		t.Fatalf("marker=%q", got)
	}
	if got := StreamRef("uid", "sandbox"); got != "container-logs/uid/sandbox" {
		t.Fatalf("stream ref=%q", got)
	}
}
