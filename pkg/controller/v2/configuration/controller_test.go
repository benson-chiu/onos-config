// SPDX-FileCopyrightText: 2020-present Open Networking Foundation <info@opennetworking.org>
//
// SPDX-License-Identifier: Apache-2.0

package configuration

import (
	"testing"

	configapi "github.com/onosproject/onos-api/go/onos/config/v2"
	"github.com/onosproject/onos-lib-go/pkg/errors"
	"github.com/stretchr/testify/assert"
)

// TestIsTransientSyncFailure pins down which southbound errors are retried.
// Retrying a change the device rejected on its merits keeps the configuration
// in SYNCHRONIZING, which blocks every later change to the same target, so a
// wrong answer here is what caused the original stall.
func TestIsTransientSyncFailure(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		transient bool
	}{
		{"device unreachable", errors.NewUnavailable("connection refused"), true},
		{"device did not answer", errors.NewTimeout("deadline exceeded"), true},
		{"attempt cut short", errors.NewCanceled("context canceled"), true},
		{"concurrent write", errors.NewConflict("conflict"), true},
		{"unclassified adapter fault", errors.NewInternal("internal"), true},

		{"device rejected the content", errors.NewInvalid("leafref unresolved"), false},
		{"path absent from the model", errors.NewNotFound("no such path"), false},
		{"operation unsupported", errors.NewNotSupported("no rpc channel"), false},
		{"entry already present", errors.NewAlreadyExists("exists"), false},
		{"credentials refused", errors.NewUnauthorized("bad credentials"), false},
		{"mastership superseded", errors.NewForbidden("not master"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.transient, isTransientSyncFailure(tt.err))
		})
	}
}

// TestSyncAttemptBudget checks that the retry budget is per index, so progress
// on one change does not consume the budget of the next.
func TestSyncAttemptBudget(t *testing.T) {
	r := &Reconciler{attempts: make(map[configapi.ConfigurationID]*syncAttempt)}
	const id = configapi.ConfigurationID("ROADM-OWS-openroadm-13.1.0")

	assert.Equal(t, 1, r.recordSyncAttempt(id, 10))
	assert.Equal(t, 2, r.recordSyncAttempt(id, 10))
	assert.Equal(t, 3, r.recordSyncAttempt(id, 10))

	// A different index starts over.
	assert.Equal(t, 1, r.recordSyncAttempt(id, 11))
	assert.Equal(t, 2, r.recordSyncAttempt(id, 11))

	// Success clears the count.
	r.clearSyncAttempt(id)
	assert.Equal(t, 1, r.recordSyncAttempt(id, 11))
}

// TestSortedIndexes states the ordering the synchronisation loop relies on.
// A failing index aborts the rest, so without a stable order a different
// subset of the configuration reaches the device on each attempt.
func TestSortedIndexes(t *testing.T) {
	got := sortedIndexes(map[configapi.Index][]*configapi.PathValue{
		321: nil, 316: nil, 9: nil, 100: nil,
	})
	assert.Equal(t, []configapi.Index{9, 100, 316, 321}, got)
}

// TestSyncFailureTypeMapping covers the codes the adapter actually returns.
func TestSyncFailureTypeMapping(t *testing.T) {
	tests := []struct {
		err  error
		want configapi.Failure_Type
	}{
		{errors.NewUnavailable("x"), configapi.Failure_UNAVAILABLE},
		{errors.NewTimeout("x"), configapi.Failure_TIMEOUT},
		{errors.NewInvalid("x"), configapi.Failure_INVALID},
		{errors.NewNotFound("x"), configapi.Failure_NOT_FOUND},
		{errors.NewInternal("x"), configapi.Failure_INTERNAL},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, syncFailureType(tt.err))
	}
}

// TestRollbackRemovesOnlyTheFailedIndex covers the rollback: the values of the
// failed transaction go and every other index is left alone.
func TestRollbackRemovesOnlyTheFailedIndex(t *testing.T) {
	applied := map[string]*configapi.PathValue{
		"/a/keep1": {Path: "/a/keep1", Index: 10},
		"/a/keep2": {Path: "/a/keep2", Index: 10},
		"/b/drop1": {Path: "/b/drop1", Index: 11},
		"/b/drop2": {Path: "/b/drop2", Index: 11},
		"/c/keep3": {Path: "/c/keep3", Index: 12},
	}

	// The loop of rollbackIndex.
	for path, value := range applied {
		if value.Index == 11 {
			delete(applied, path)
		}
	}

	assert.Len(t, applied, 3)
	for _, path := range []string{"/a/keep1", "/a/keep2", "/c/keep3"} {
		assert.Contains(t, applied, path)
	}
	for _, path := range []string{"/b/drop1", "/b/drop2"} {
		assert.NotContains(t, applied, path)
	}
}
