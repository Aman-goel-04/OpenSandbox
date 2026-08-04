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

// Package api contains the stable extension contracts shared by Node Agent
// Sources, the Pipeline, and Sinks.
package api

import (
	"context"
	"errors"
	"time"
)

type RecordKind string

const RecordKindContainerLog RecordKind = "container-log"

type Capabilities struct {
	RecordKinds []RecordKind
}

type Resource struct {
	SandboxID    string `json:"sandbox_id"`
	ClusterName  string `json:"k8s.cluster.name"`
	Namespace    string `json:"k8s.namespace.name"`
	PodName      string `json:"k8s.pod.name"`
	PodUID       string `json:"k8s.pod.uid"`
	NodeName     string `json:"k8s.node.name"`
	Container    string `json:"k8s.container.name"`
	LogDirectory string `json:"log_directory,omitempty"`
	Terminated   bool   `json:"-"`
}

type Record struct {
	Kind       RecordKind        `json:"kind"`
	Timestamp  time.Time         `json:"timestamp"`
	Body       []byte            `json:"body"`
	Resource   Resource          `json:"resource"`
	Attributes map[string]string `json:"attributes"`
}

type StreamRef struct {
	ID string `json:"id"`
}

type AckToken struct {
	ID        string    `json:"id"`
	Source    string    `json:"source"`
	StreamRef StreamRef `json:"stream_ref"`
	Value     []byte    `json:"value"`
}

type SourcePositionToken struct {
	ID        string    `json:"id"`
	Source    string    `json:"source"`
	StreamRef StreamRef `json:"stream_ref"`
	Value     []byte    `json:"value"`
}

type EndToken struct {
	ID        string    `json:"id"`
	Source    string    `json:"source"`
	StreamRef StreamRef `json:"stream_ref"`
	Value     []byte    `json:"value"`
}

type AckDisposition string

const (
	AckDelivered       AckDisposition = "delivered"
	AckIntentionalDrop AckDisposition = "intentional-drop"
)

type DeliveryGuarantee string

const (
	GuaranteeDurable    DeliveryGuarantee = "durable"
	GuaranteeBestEffort DeliveryGuarantee = "best-effort"
)

type AckResult struct {
	Token       AckToken          `json:"token"`
	Disposition AckDisposition    `json:"disposition"`
	Reason      string            `json:"reason,omitempty"`
	Guarantee   DeliveryGuarantee `json:"guarantee"`
}

type SourceOutcome struct {
	HadDrops      bool     `json:"had_drops"`
	HadSourceGaps bool     `json:"had_source_gaps"`
	LossReasons   []string `json:"loss_reasons"`
}

type Delivery struct {
	Record    Record
	StreamRef StreamRef
	AckToken  AckToken
	RecordID  string
}

type StreamEnd struct {
	StreamRef         StreamRef
	EndToken          EndToken
	TerminalWatermark *SourcePositionToken
	Revision          uint64
	CoverageStartedAt time.Time
	Resource          Resource
	Outcome           SourceOutcome
}

type SourceEvent struct {
	Delivery *Delivery
	End      *StreamEnd
}

func (e SourceEvent) Valid() bool {
	return (e.Delivery == nil) != (e.End == nil)
}

type Source interface {
	Name() string
	Capabilities() Capabilities
	Start(context.Context, chan<- SourceEvent) error
	Acknowledge(context.Context, []AckResult) error
	AcknowledgeEnd(context.Context, EndToken) error
	Stop(context.Context) error
}

// SourceError optionally classifies whether retrying the same Source
// acknowledgement without changing its token can succeed. Unclassified
// errors are retryable for backward compatibility with custom Sources.
type SourceError interface {
	error
	Retryable() bool
}

// IsRetryableSourceError reports whether a failed Source operation may be
// retried. It returns false for nil because there is no failed operation to
// retry.
func IsRetryableSourceError(err error) bool {
	if err == nil {
		return false
	}
	var sourceErr SourceError
	return !errors.As(err, &sourceErr) || sourceErr.Retryable()
}

type BatchItem struct {
	Record   Record
	RecordID string
}

type Batch struct {
	StreamRef StreamRef
	Items     []BatchItem
}

type FinalizeRequest struct {
	FinalizeID        string
	TargetID          string
	StreamRef         StreamRef
	Revision          uint64
	CoverageStartedAt time.Time
	Resource          Resource
	Outcome           SourceOutcome
	FinalizedAt       time.Time
}

type Sink interface {
	Name() string
	Capabilities() Capabilities
	Guarantee() DeliveryGuarantee
	Consume(context.Context, Batch) error
	Finalize(context.Context, FinalizeRequest) error
	Close(context.Context) error
}

// SinkError optionally classifies whether retrying the same Sink operation
// without changing its inputs or configuration can succeed. Unclassified
// errors are retryable for backward compatibility with custom Sinks.
type SinkError interface {
	error
	Retryable() bool
}

// IsRetryableSinkError reports whether a failed Sink operation may be retried.
// It returns false for nil because there is no failed operation to retry.
func IsRetryableSinkError(err error) bool {
	if err == nil {
		return false
	}
	var sinkErr SinkError
	return !errors.As(err, &sinkErr) || sinkErr.Retryable()
}
