// SPDX-FileCopyrightText: 2020-present Open Networking Foundation <info@opennetworking.org>
//
// SPDX-License-Identifier: Apache-2.0

package proposal

import (
	"context"
	liberrors "github.com/onosproject/onos-lib-go/pkg/errors"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"strings"
	"testing"

	"github.com/golang/mock/gomock"
	configapi "github.com/onosproject/onos-api/go/onos/config/v2"
	configurationmock "github.com/onosproject/onos-config/internal/store/v2/configuration"
	proposalmock "github.com/onosproject/onos-config/internal/store/v2/proposal"
	"github.com/onosproject/onos-config/pkg/store/v2/configuration"
	"github.com/onosproject/onos-lib-go/pkg/errors"
	"github.com/stretchr/testify/assert"
)

const (
	testTargetID      = configapi.TargetID("test-target")
	testTargetType    = configapi.TargetType("test-type")
	testTargetVersion = configapi.TargetVersion("1.0.0")
)

// abortTestCase describes one starting state of a Configuration whose Proposal
// is entering the abort phase.
type abortTestCase struct {
	name string

	// Configuration state before the abort is reconciled.
	committedIndex configapi.Index
	appliedIndex   configapi.Index

	// Proposal being aborted.
	transactionIndex configapi.Index
	prevIndex        configapi.Index

	// Expected Configuration state after the abort is reconciled.
	wantCommittedIndex configapi.Index
	wantAppliedIndex   configapi.Index

	// Whether the proposal is expected to reach the ABORTED state. A proposal
	// that is left unfinished is retried on a later reconcile.
	wantAborted bool
}

// newAbortProposal builds a proposal sitting in the ABORTING state.
func newAbortProposal(transactionIndex, prevIndex configapi.Index) *configapi.Proposal {
	return &configapi.Proposal{
		ObjectMeta: configapi.ObjectMeta{Revision: 1},
		ID: configapi.ProposalID(
			string(testTargetID) + "-" + string(rune('0'+transactionIndex%10))),
		TargetID: testTargetID,
		TargetTypeVersion: configapi.TargetTypeVersion{
			TargetType:    testTargetType,
			TargetVersion: testTargetVersion,
		},
		TransactionIndex: transactionIndex,
		Details: &configapi.Proposal_Change{
			Change: &configapi.ChangeProposal{
				Values: map[string]*configapi.PathValue{},
			},
		},
		Status: configapi.ProposalStatus{
			PrevIndex: prevIndex,
			Phases: configapi.ProposalPhases{
				Abort: &configapi.ProposalAbortPhase{
					State: configapi.ProposalAbortPhase_ABORTING,
				},
			},
		},
	}
}

// newConfiguration builds a Configuration with the given committed/applied indexes.
func newConfiguration(committedIndex, appliedIndex configapi.Index) *configapi.Configuration {
	return &configapi.Configuration{
		ObjectMeta: configapi.ObjectMeta{Revision: 1},
		ID:         configuration.NewID(testTargetID, testTargetType, testTargetVersion),
		TargetID:   testTargetID,
		Status: configapi.ConfigurationStatus{
			Committed: configapi.CommittedConfigurationStatus{Index: committedIndex},
			Applied:   configapi.AppliedConfigurationStatus{Index: appliedIndex},
		},
	}
}

// TestReconcileAbortAdvancesIndexes covers every branch of reconcileAbort.
//
// A proposal that is marked ABORTED must leave both the committed and the
// applied index at or beyond its own transaction index. Any proposal that is
// finalized while an index still trails behind blocks every later proposal
// forever, because reconcileApply waits for Applied.Index to equal its
// PrevIndex and nothing will ever advance it again.
func TestReconcileAbortAdvancesIndexes(t *testing.T) {
	tests := []abortTestCase{
		{
			// Both indexes are at PrevIndex: the straightforward case.
			name:               "both indexes at prev index",
			committedIndex:     105,
			appliedIndex:       105,
			transactionIndex:   106,
			prevIndex:          105,
			wantCommittedIndex: 106,
			wantAppliedIndex:   106,
			wantAborted:        true,
		},
		{
			// Committed has reached PrevIndex but applied has not. Only the
			// committed index can move; the proposal stays unfinished and is
			// retried once the applied index catches up.
			name:               "applied index still behind",
			committedIndex:     105,
			appliedIndex:       104,
			transactionIndex:   106,
			prevIndex:          105,
			wantCommittedIndex: 106,
			wantAppliedIndex:   104,
			wantAborted:        false,
		},
		{
			// Regression test for the deadlock seen in the field: a failed
			// rollback at index 106 was aborted while transactions 107 and 108
			// had already been committed, so Committed.Index had run ahead.
			// Applied.Index was left at 105 and transactions 107 and 108 waited
			// for index 106 forever.
			name:               "committed index ahead of transaction index",
			committedIndex:     108,
			appliedIndex:       105,
			transactionIndex:   106,
			prevIndex:          105,
			wantCommittedIndex: 106,
			wantAppliedIndex:   106,
			wantAborted:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			config := newConfiguration(tt.committedIndex, tt.appliedIndex)
			proposal := newAbortProposal(tt.transactionIndex, tt.prevIndex)

			configurations := configurationmock.NewMockStore(ctrl)
			configurations.EXPECT().
				Get(gomock.Any(), config.ID).
				Return(config, nil).
				AnyTimes()
			configurations.EXPECT().
				UpdateStatus(gomock.Any(), gomock.Any()).
				DoAndReturn(func(_ context.Context, updated *configapi.Configuration) error {
					config = updated
					return nil
				}).
				AnyTimes()

			proposals := proposalmock.NewMockStore(ctrl)
			proposals.EXPECT().
				UpdateStatus(gomock.Any(), gomock.Any()).
				DoAndReturn(func(_ context.Context, updated *configapi.Proposal) error {
					proposal = updated
					return nil
				}).
				AnyTimes()

			reconciler := &Reconciler{
				proposals:      proposals,
				configurations: configurations,
			}

			_, err := reconciler.reconcileAbort(context.Background(), proposal)
			assert.NoError(t, err)

			assert.Equal(t, tt.wantCommittedIndex, config.Status.Committed.Index,
				"committed index")
			assert.Equal(t, tt.wantAppliedIndex, config.Status.Applied.Index,
				"applied index")

			if tt.wantAborted {
				assert.Equal(t, configapi.ProposalAbortPhase_ABORTED,
					proposal.Status.Phases.Abort.State,
					"proposal should reach the ABORTED state")

				// The invariant that matters: a finalized proposal must never
				// leave a later proposal waiting on an index that will never
				// be reached.
				assert.GreaterOrEqual(t, config.Status.Applied.Index, tt.transactionIndex,
					"an ABORTED proposal must not leave Applied.Index behind its own index")
			} else {
				assert.Equal(t, configapi.ProposalAbortPhase_ABORTING,
					proposal.Status.Phases.Abort.State,
					"proposal should stay unfinished so it is retried")
			}
		})
	}
}

// newFinalizedProposal builds a proposal that will never be reconciled again.
func newFinalizedProposal(transactionIndex configapi.Index, abort configapi.ProposalAbortPhase_State, apply configapi.ProposalApplyPhase_State) *configapi.Proposal {
	proposal := &configapi.Proposal{
		ObjectMeta:       configapi.ObjectMeta{Revision: 1},
		TargetID:         testTargetID,
		TransactionIndex: transactionIndex,
	}
	if abort != 0 {
		proposal.Status.Phases.Abort = &configapi.ProposalAbortPhase{State: abort}
	}
	if apply != 0 {
		proposal.Status.Phases.Apply = &configapi.ProposalApplyPhase{State: apply}
	}
	return proposal
}

// TestRepairStalledApplyIndex covers the self-healing path that releases a
// target stalled behind a finalized proposal.
//
// This is the recovery counterpart to the abort fix: a proposal finalized by an
// older build may already have left the applied index behind, and no reconcile
// of that proposal will ever advance it again.
func TestRepairStalledApplyIndex(t *testing.T) {
	tests := []struct {
		name string

		appliedIndex configapi.Index
		prevIndex    configapi.Index

		// Predecessor state. A nil predecessor simulates a missing proposal.
		prevAbort   configapi.ProposalAbortPhase_State
		prevApply   configapi.ProposalApplyPhase_State
		prevMissing bool

		wantRepaired     bool
		wantAppliedIndex configapi.Index
	}{
		{
			// The field deadlock: an aborted rollback left the index behind.
			name:             "predecessor aborted",
			appliedIndex:     105,
			prevIndex:        106,
			prevAbort:        configapi.ProposalAbortPhase_ABORTED,
			wantRepaired:     true,
			wantAppliedIndex: 106,
		},
		{
			name:             "predecessor apply failed",
			appliedIndex:     105,
			prevIndex:        106,
			prevApply:        configapi.ProposalApplyPhase_FAILED,
			wantRepaired:     true,
			wantAppliedIndex: 106,
		},
		{
			name:             "predecessor applied but index left behind",
			appliedIndex:     105,
			prevIndex:        106,
			prevApply:        configapi.ProposalApplyPhase_APPLIED,
			wantRepaired:     true,
			wantAppliedIndex: 106,
		},
		{
			// A predecessor still applying will advance the index itself, so
			// waiting is correct here.
			name:             "predecessor still applying",
			appliedIndex:     105,
			prevIndex:        106,
			prevApply:        configapi.ProposalApplyPhase_APPLYING,
			wantRepaired:     false,
			wantAppliedIndex: 105,
		},
		{
			name:             "predecessor still aborting",
			appliedIndex:     105,
			prevIndex:        106,
			prevAbort:        configapi.ProposalAbortPhase_ABORTING,
			wantRepaired:     false,
			wantAppliedIndex: 105,
		},
		{
			name:             "predecessor missing",
			appliedIndex:     105,
			prevIndex:        106,
			prevMissing:      true,
			wantRepaired:     true,
			wantAppliedIndex: 106,
		},
		{
			// Nothing to repair when the index is already ahead.
			name:             "applied index already ahead",
			appliedIndex:     107,
			prevIndex:        106,
			prevAbort:        configapi.ProposalAbortPhase_ABORTED,
			wantRepaired:     false,
			wantAppliedIndex: 107,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			config := newConfiguration(tt.prevIndex, tt.appliedIndex)
			proposal := newAbortProposal(tt.prevIndex+1, tt.prevIndex)

			configurations := configurationmock.NewMockStore(ctrl)
			configurations.EXPECT().
				UpdateStatus(gomock.Any(), gomock.Any()).
				Return(nil).
				AnyTimes()

			proposals := proposalmock.NewMockStore(ctrl)
			if tt.prevMissing {
				proposals.EXPECT().
					Get(gomock.Any(), gomock.Any()).
					Return(nil, errors.NewNotFound("not found")).
					AnyTimes()
			} else {
				proposals.EXPECT().
					Get(gomock.Any(), gomock.Any()).
					Return(newFinalizedProposal(tt.prevIndex, tt.prevAbort, tt.prevApply), nil).
					AnyTimes()
			}

			reconciler := &Reconciler{
				proposals:      proposals,
				configurations: configurations,
			}

			repaired, err := reconciler.repairStalledApplyIndex(
				context.Background(), config, proposal)
			assert.NoError(t, err)
			assert.Equal(t, tt.wantRepaired, repaired, "repaired")
			assert.Equal(t, tt.wantAppliedIndex, config.Status.Applied.Index,
				"applied index")
		})
	}
}

// TestApplyFailureType covers the classification the northbound relies on to
// turn a failed transaction back into a gRPC status.
//
// The inputs go through errors.FromGRPC first, because that is what the
// southbound client hands to the reconciler. Reading the code with
// status.Code at that point yields Unknown for every one of them, which is
// why these are classified with the onos-lib-go predicates instead.
func TestApplyFailureType(t *testing.T) {
	tests := []struct {
		code codes.Code
		want configapi.Failure_Type
	}{
		{codes.InvalidArgument, configapi.Failure_INVALID},
		{codes.Unavailable, configapi.Failure_UNAVAILABLE},
		{codes.DeadlineExceeded, configapi.Failure_TIMEOUT},
		{codes.NotFound, configapi.Failure_NOT_FOUND},
		{codes.AlreadyExists, configapi.Failure_ALREADY_EXISTS},
		{codes.PermissionDenied, configapi.Failure_FORBIDDEN},
		{codes.Unauthenticated, configapi.Failure_UNAUTHORIZED},
		{codes.FailedPrecondition, configapi.Failure_CONFLICT},
		{codes.Unimplemented, configapi.Failure_NOT_SUPPORTED},
		{codes.Internal, configapi.Failure_INTERNAL},
		{codes.Canceled, configapi.Failure_CANCELED},
		{codes.Unknown, configapi.Failure_UNKNOWN},
	}

	for _, tt := range tests {
		t.Run(tt.code.String(), func(t *testing.T) {
			southbound := liberrors.FromGRPC(status.Error(tt.code, "device said no"))

			// Guard the premise: if this ever starts reporting the original
			// code, the predicates below are no longer the only way to read it.
			if got := status.Code(southbound); got == tt.code && tt.code != codes.Unknown {
				t.Logf("status.Code 現在回得出 %v，可考慮簡化分類", got)
			}

			if got := applyFailureType(southbound); got != tt.want {
				t.Errorf("applyFailureType(%v) = %v，預期 %v", tt.code, got, tt.want)
			}
		})
	}
}

// TestApplyFailureTypeRejectsUnknownOnly makes sure the mapping is not
// accidentally collapsing everything to one value, which is the bug this
// replaced.
func TestApplyFailureTypeRejectsUnknownOnly(t *testing.T) {
	seen := make(map[configapi.Failure_Type]struct{})
	for _, code := range []codes.Code{
		codes.InvalidArgument, codes.Unavailable, codes.DeadlineExceeded,
		codes.NotFound, codes.Internal,
	} {
		seen[applyFailureType(liberrors.FromGRPC(status.Error(code, "x")))] = struct{}{}
	}
	if len(seen) != 5 {
		t.Errorf("五種錯誤只對應到 %d 種 failure type：%v", len(seen), seen)
	}
	if _, ok := seen[configapi.Failure_UNKNOWN]; ok {
		t.Error("已分類的錯誤不該落到 UNKNOWN")
	}
}

// TestRecordApplyRetry covers what an application needs while a target is
// unreachable: the reason has to reach the proposal, and the apply phase has
// to stay open so the controller keeps retrying.
func TestRecordApplyRetry(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	proposal := newAbortProposal(106, 105)
	proposal.Status.Phases.Apply = &configapi.ProposalApplyPhase{
		State: configapi.ProposalApplyPhase_APPLYING,
	}

	var writes int
	proposals := proposalmock.NewMockStore(ctrl)
	proposals.EXPECT().
		UpdateStatus(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, updated *configapi.Proposal) error {
			writes++
			proposal = updated
			return nil
		}).
		AnyTimes()

	reconciler := &Reconciler{proposals: proposals}
	cause := liberrors.FromGRPC(status.Error(codes.Unavailable,
		"與設備的連線失敗: dial tcp 10.0.0.1:830: connect: connection refused"))

	if err := reconciler.recordApplyRetry(context.Background(), proposal, cause); err != nil {
		t.Fatalf("recordApplyRetry: %v", err)
	}

	failure := proposal.Status.Phases.Apply.Failure
	if failure == nil {
		t.Fatal("重試的原因沒有被記錄")
	}
	if failure.Type != configapi.Failure_UNAVAILABLE {
		t.Errorf("failure type = %v，預期 UNAVAILABLE", failure.Type)
	}
	if !strings.HasPrefix(failure.Description, applyRetryPrefix) {
		t.Errorf("描述少了 %q 前綴，會被誤認為最終失敗：%s",
			applyRetryPrefix, failure.Description)
	}
	if !strings.Contains(failure.Description, "connection refused") {
		t.Errorf("描述少了設備的原文：%s", failure.Description)
	}

	// The phase must stay open: ending it here would stop the retry.
	if got := proposal.Status.Phases.Apply.State; got != configapi.ProposalApplyPhase_APPLYING {
		t.Errorf("apply phase = %v，預期仍是 APPLYING", got)
	}

	// Same cause again must not write: a device that stays down would
	// otherwise emit one store update per retry forever.
	before := writes
	if err := reconciler.recordApplyRetry(context.Background(), proposal, cause); err != nil {
		t.Fatalf("第二次 recordApplyRetry: %v", err)
	}
	if writes != before {
		t.Errorf("原因相同時不該重複寫入，寫了 %d 次", writes-before)
	}

	// A different cause must write.
	other := liberrors.FromGRPC(status.Error(codes.DeadlineExceeded, "等設備回應逾時"))
	if err := reconciler.recordApplyRetry(context.Background(), proposal, other); err != nil {
		t.Fatalf("換原因後 recordApplyRetry: %v", err)
	}
	if writes == before {
		t.Error("原因改變時應該寫入")
	}
	if got := proposal.Status.Phases.Apply.Failure.Type; got != configapi.Failure_TIMEOUT {
		t.Errorf("換原因後 failure type = %v，預期 TIMEOUT", got)
	}
}
