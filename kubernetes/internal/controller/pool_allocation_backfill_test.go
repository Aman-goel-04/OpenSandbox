// Copyright 2025 Alibaba Group Holding Ltd.
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

package controller

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

func TestBackfillLegacyPoolAllocation(t *testing.T) {
	ctx := context.Background()
	pool := &sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default"}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      "pool-pod",
		Namespace: "default",
		Labels:    map[string]string{LabelPoolName: pool.Name},
	}}

	t.Run("success stamps exact record and is idempotent", func(t *testing.T) {
		sandbox := newLegacyAllocationSandbox("sandbox", pool.Name)
		r := newBackfillTestReconciler(t, sandbox)

		if err := r.backfillLegacyPoolAllocation(ctx, pool, sandbox, []*corev1.Pod{pod}); err != nil {
			t.Fatalf("backfillLegacyPoolAllocation() error = %v", err)
		}
		updated := getBackfillSandbox(t, ctx, r, sandbox)
		allocation := parseBackfillAllocation(t, updated)
		want := SandboxAllocation{Pods: []string{"pool-pod"}, PoolRef: pool.Name, Generation: sandbox.Generation}
		if !reflect.DeepEqual(allocation, want) {
			t.Fatalf("allocation = %#v, want %#v", allocation, want)
		}
		firstAnnotations := updated.GetAnnotations()
		firstResourceVersion := updated.ResourceVersion

		if err := r.backfillLegacyPoolAllocation(ctx, pool, updated, []*corev1.Pod{pod}); err != nil {
			t.Fatalf("second backfillLegacyPoolAllocation() error = %v", err)
		}
		again := getBackfillSandbox(t, ctx, r, sandbox)
		if !reflect.DeepEqual(again.GetAnnotations(), firstAnnotations) {
			t.Fatalf("second backfill changed annotations: got %#v, want %#v", again.GetAnnotations(), firstAnnotations)
		}
		if again.ResourceVersion != firstResourceVersion {
			t.Fatalf("second backfill changed resource version: got %q, want %q", again.ResourceVersion, firstResourceVersion)
		}
	})

	tests := []struct {
		name   string
		mutate func(*sandboxv1alpha1.BatchSandbox)
		pods   []*corev1.Pod
	}{
		{
			name: "missing pool pod",
			pods: nil,
		},
		{
			name: "release intersects allocation",
			mutate: func(sandbox *sandboxv1alpha1.BatchSandbox) {
				sandbox.Annotations[AnnoAllocReleaseKey] = `{"pods":["pool-pod"]}`
			},
			pods: []*corev1.Pod{pod},
		},
		{
			name: "malformed release",
			mutate: func(sandbox *sandboxv1alpha1.BatchSandbox) {
				sandbox.Annotations[AnnoAllocReleaseKey] = `{`
			},
			pods: []*corev1.Pod{pod},
		},
		{
			name: "release missing pods",
			mutate: func(sandbox *sandboxv1alpha1.BatchSandbox) {
				sandbox.Annotations[AnnoAllocReleaseKey] = `{}`
			},
			pods: []*corev1.Pod{pod},
		},
		{
			name: "release null pods",
			mutate: func(sandbox *sandboxv1alpha1.BatchSandbox) {
				sandbox.Annotations[AnnoAllocReleaseKey] = `{"pods":null}`
			},
			pods: []*corev1.Pod{pod},
		},
		{
			name: "release duplicate pod",
			mutate: func(sandbox *sandboxv1alpha1.BatchSandbox) {
				sandbox.Annotations[AnnoAllocReleaseKey] = `{"pods":["released-pod","released-pod"]}`
			},
			pods: []*corev1.Pod{pod},
		},
		{
			name: "malformed released",
			mutate: func(sandbox *sandboxv1alpha1.BatchSandbox) {
				sandbox.Annotations[AnnoAllocReleasedKey] = `{`
			},
			pods: []*corev1.Pod{pod},
		},
		{
			name: "released missing pods",
			mutate: func(sandbox *sandboxv1alpha1.BatchSandbox) {
				sandbox.Annotations[AnnoAllocReleasedKey] = `{}`
			},
			pods: []*corev1.Pod{pod},
		},
		{
			name: "released invalid pod",
			mutate: func(sandbox *sandboxv1alpha1.BatchSandbox) {
				sandbox.Annotations[AnnoAllocReleasedKey] = `{"pods":["INVALID_POD"]}`
			},
			pods: []*corev1.Pod{pod},
		},
		{
			name: "deleting sandbox",
			mutate: func(sandbox *sandboxv1alpha1.BatchSandbox) {
				now := metav1.NewTime(time.Now())
				sandbox.DeletionTimestamp = &now
				sandbox.Finalizers = append(sandbox.Finalizers, "keep-deleting-object")
			},
			pods: []*corev1.Pod{pod},
		},
		{
			name: "missing allocation finalizer",
			mutate: func(sandbox *sandboxv1alpha1.BatchSandbox) {
				sandbox.Finalizers = nil
			},
			pods: []*corev1.Pod{pod},
		},
		{
			name: "nonempty mismatched pool ref",
			mutate: func(sandbox *sandboxv1alpha1.BatchSandbox) {
				sandbox.Annotations[AnnoAllocStatusKey] = `{"pods":["pool-pod"],"poolRef":"other-pool","generation":1}`
			},
			pods: []*corev1.Pod{pod},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandbox := newLegacyAllocationSandbox("sandbox", pool.Name)
			if tt.mutate != nil {
				tt.mutate(sandbox)
			}
			original := sandbox.Annotations[AnnoAllocStatusKey]
			r := newBackfillTestReconciler(t, sandbox)

			if err := r.backfillLegacyPoolAllocation(ctx, pool, sandbox, tt.pods); err != nil {
				t.Fatalf("backfillLegacyPoolAllocation() error = %v", err)
			}
			updated := getBackfillSandbox(t, ctx, r, sandbox)
			if got := updated.Annotations[AnnoAllocStatusKey]; got != original {
				t.Fatalf("alloc-status = %q, want unchanged %q", got, original)
			}
		})
	}
}

func newLegacyAllocationSandbox(name, poolRef string) *sandboxv1alpha1.BatchSandbox {
	return &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "default",
			Generation:  7,
			Finalizers:  []string{FinalizerPoolAllocation},
			Annotations: map[string]string{AnnoAllocStatusKey: `{"pods":["pool-pod"]}`},
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{PoolRef: poolRef},
	}
}

func newBackfillTestReconciler(t *testing.T, objects ...runtime.Object) *PoolReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := sandboxv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return &PoolReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).Build()}
}

func getBackfillSandbox(t *testing.T, ctx context.Context, r *PoolReconciler, sandbox *sandboxv1alpha1.BatchSandbox) *sandboxv1alpha1.BatchSandbox {
	t.Helper()
	updated := &sandboxv1alpha1.BatchSandbox{}
	if err := r.Get(ctx, types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}, updated); err != nil {
		t.Fatal(err)
	}
	return updated
}

func parseBackfillAllocation(t *testing.T, sandbox *sandboxv1alpha1.BatchSandbox) SandboxAllocation {
	t.Helper()
	allocation := SandboxAllocation{}
	if err := json.Unmarshal([]byte(sandbox.Annotations[AnnoAllocStatusKey]), &allocation); err != nil {
		t.Fatal(err)
	}
	return allocation
}
