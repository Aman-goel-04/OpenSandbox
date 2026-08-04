// Copyright 2026 Alibaba Group Holding Ltd.
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"fmt"
	"testing"
)

type classifiedSinkError struct {
	retryable bool
}

func (e classifiedSinkError) Error() string   { return "classified sink error" }
func (e classifiedSinkError) Retryable() bool { return e.retryable }

type classifiedSourceError struct {
	retryable bool
}

func (e classifiedSourceError) Error() string   { return "classified source error" }
func (e classifiedSourceError) Retryable() bool { return e.retryable }

func TestIsRetryableSourceError(t *testing.T) {
	if IsRetryableSourceError(nil) {
		t.Fatal("nil error must not be retryable")
	}
	if !IsRetryableSourceError(errors.New("unclassified")) {
		t.Fatal("unclassified custom source errors must remain retryable")
	}
	if !IsRetryableSourceError(classifiedSourceError{retryable: true}) {
		t.Fatal("retryable classified error was rejected")
	}
	if IsRetryableSourceError(fmt.Errorf("wrapped: %w", classifiedSourceError{})) {
		t.Fatal("wrapped non-retryable error was not preserved")
	}
}

func TestIsRetryableSinkError(t *testing.T) {
	if IsRetryableSinkError(nil) {
		t.Fatal("nil error must not be retryable")
	}
	if !IsRetryableSinkError(errors.New("unclassified")) {
		t.Fatal("unclassified custom sink errors must remain retryable")
	}
	if !IsRetryableSinkError(classifiedSinkError{retryable: true}) {
		t.Fatal("retryable classified error was rejected")
	}
	if IsRetryableSinkError(fmt.Errorf("wrapped: %w", classifiedSinkError{})) {
		t.Fatal("wrapped non-retryable error was not preserved")
	}
}
