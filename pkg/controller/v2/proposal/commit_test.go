// SPDX-FileCopyrightText: 2020-present Open Networking Foundation <info@opennetworking.org>
//
// SPDX-License-Identifier: Apache-2.0

package proposal

import (
	"testing"

	configapi "github.com/onosproject/onos-api/go/onos/config/v2"
	"github.com/stretchr/testify/assert"
)

const testIfPath = "/org-openroadm-device/interface[name=nmc-ctp-ch-1]"

// commitChange applies a change the way reconcileCommit does.
func commitChange(store, change map[string]*configapi.PathValue) {
	for _, path := range sortedPaths(change) {
		_, _ = applyChangeToConfig(store, path, change[path])
	}
}

// TestCommitOrderClearsTombstone covers the sequence that left the store out of
// step with the device: a list entry is deleted and then recreated. The
// tombstone of the entry has to be cleared by the paths that replace it, which
// only happens if ancestors are applied first. Map iteration order used to
// decide this, so the same change could either land or be swallowed.
func TestCommitOrderClearsTombstone(t *testing.T) {
	leaves := []string{"/name", "/type", "/administrative-state",
		"/supporting-circuit-pack-name", "/supporting-port",
		"/nmc-ctp/frequency", "/nmc-ctp/width"}

	// One change carrying both the tombstone and the paths recreating the entry.
	for i := 0; i < 200; i++ {
		change := map[string]*configapi.PathValue{
			testIfPath: {Path: testIfPath, Deleted: true, Index: 1},
		}
		for _, leaf := range leaves {
			change[testIfPath+leaf] = &configapi.PathValue{Path: testIfPath + leaf, Index: 2}
		}

		store := map[string]*configapi.PathValue{}
		commitChange(store, change)

		assert.NotContains(t, store, testIfPath,
			"the tombstone must not survive the paths that recreate the entry")
		for _, leaf := range leaves {
			assert.Contains(t, store, testIfPath+leaf)
		}
	}
}

// TestCommitClearsTombstoneFromEarlierChange covers the observed case: the
// delete and the recreation arrive as separate transactions, so the tombstone
// is already in the store.
func TestCommitClearsTombstoneFromEarlierChange(t *testing.T) {
	store := map[string]*configapi.PathValue{}

	// Transaction 1 deletes the entry.
	commitChange(store, map[string]*configapi.PathValue{
		testIfPath: {Path: testIfPath, Deleted: true, Index: 1},
	})
	assert.Contains(t, store, testIfPath)
	assert.True(t, store[testIfPath].Deleted)

	// Transaction 2 recreates it.
	commitChange(store, map[string]*configapi.PathValue{
		testIfPath + "/type":            {Path: testIfPath + "/type", Index: 2},
		testIfPath + "/supporting-port": {Path: testIfPath + "/supporting-port", Index: 2},
	})

	assert.NotContains(t, store, testIfPath, "the tombstone must be cleared")
	assert.Contains(t, store, testIfPath+"/type")
	assert.Contains(t, store, testIfPath+"/supporting-port")
}

// TestCommitClearsTombstoneWithSlashesInKey is the same case for an OpenROADM
// hardware name. GetParentPath used to split inside the key, so no ancestor
// ever matched and the tombstone stayed.
func TestCommitClearsTombstoneWithSlashesInKey(t *testing.T) {
	const path = "/org-openroadm-device/interface[name=ots-1/0/0/E1]"
	store := map[string]*configapi.PathValue{
		path: {Path: path, Deleted: true, Index: 1},
	}

	commitChange(store, map[string]*configapi.PathValue{
		path + "/type": {Path: path + "/type", Index: 2},
	})

	assert.NotContains(t, store, path, "the tombstone must be cleared")
	assert.Contains(t, store, path+"/type")
}

// TestSortedPathsOrdersAncestorsFirst states the property the commit loop
// depends on.
func TestSortedPathsOrdersAncestorsFirst(t *testing.T) {
	got := sortedPaths(map[string]*configapi.PathValue{
		testIfPath + "/nmc-ctp/frequency": {},
		testIfPath:                        {},
		testIfPath + "/type":              {},
		testIfPath + "/nmc-ctp":           {},
	})
	assert.Equal(t, []string{
		testIfPath,
		testIfPath + "/nmc-ctp",
		testIfPath + "/nmc-ctp/frequency",
		testIfPath + "/type",
	}, got)
}
