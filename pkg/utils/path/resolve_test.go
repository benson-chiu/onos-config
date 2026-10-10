// Copyright 2026-present Open Networking Foundation.
//
// SPDX-License-Identifier: Apache-2.0

package path

import (
	"testing"

	"github.com/onosproject/onos-api/go/onos/config/admin"
	"github.com/stretchr/testify/assert"
)

// openroadmRWPaths is a slice of the real OpenROADM read-write map, enough to
// exercise containers, list keys with slashes in them, and augmented subtrees.
var openroadmRWPaths = ReadWritePathMap{
	"/org-openroadm-device/info/clli":                         {ValueType: 4},
	"/org-openroadm-device/info/node-id":                      {ValueType: 4},
	"/org-openroadm-device/interface/name":                    {ValueType: 4, IsAKey: true},
	"/org-openroadm-device/interface/type":                    {ValueType: 4},
	"/org-openroadm-device/interface/administrative-state":    {ValueType: 4},
	"/org-openroadm-device/interface/supporting-port":         {ValueType: 4},
	"/org-openroadm-device/interface/nmc-ctp/frequency":       {ValueType: 7},
	"/org-openroadm-device/interface/mc-ttp/min-freq":         {ValueType: 7},
	"/org-openroadm-device/circuit-packs/circuit-pack-name":   {ValueType: 4, IsAKey: true},
	"/org-openroadm-device/circuit-packs/ports/port-name":     {ValueType: 4, IsAKey: true},
	"/org-openroadm-device/roadm-connections/connection-name": {ValueType: 4, IsAKey: true},
	"/org-openroadm-device/roadm-connections/source/src-if":   {ValueType: 4},
	// Deliberately adjacent to /info/clli, to catch prefix matching that
	// does not check the segment boundary.
	"/org-openroadm-device/info-extra/clli-label": {ValueType: 4},
}

var openroadmROPaths = ReadOnlyPathMap{
	"/active-alarm-list/activeAlarms": ReadOnlySubPathMap{
		"/severity":  admin.ReadOnlySubPath{ValueType: 4},
		"/raiseTime": admin.ReadOnlySubPath{ValueType: 4},
	},
	"/current-pm-list/current-pm-entry": ReadOnlySubPathMap{
		"/pm-resource-type": admin.ReadOnlySubPath{ValueType: 4},
	},
	"/org-openroadm-device/info/current-datetime": ReadOnlySubPathMap{
		"/": admin.ReadOnlySubPath{ValueType: 4},
	},
}

func TestStripModulePrefixes(t *testing.T) {
	cases := []struct{ in, want string }{{
		in:   "/org-openroadm-device:org-openroadm-device/info/clli",
		want: "/org-openroadm-device/info/clli",
	}, {
		// Augment prefix mid-path.
		in:   "/org-openroadm-device:org-openroadm-device/interface[name=x]/org-openroadm-network-media-channel-interfaces:nmc-ctp/frequency",
		want: "/org-openroadm-device/interface[name=x]/nmc-ctp/frequency",
	}, {
		// A key value containing a slash must survive intact.
		in:   "/org-openroadm-device:org-openroadm-device/interface[name=ots-1/0/0/E1]/type",
		want: "/org-openroadm-device/interface[name=ots-1/0/0/E1]/type",
	}, {
		// A colon inside a key value is part of the value, not a prefix.
		in:   "/org-openroadm-device/interface[name=a:b]/type",
		want: "/org-openroadm-device/interface[name=a:b]/type",
	}, {
		in:   "/org-openroadm-device/info/clli", // already bare
		want: "/org-openroadm-device/info/clli",
	}}

	for _, tc := range cases {
		assert.Equal(t, tc.want, StripModulePrefixes(tc.in), "StripModulePrefixes(%q)", tc.in)
	}
}

// TestResolveModelPathAcceptsPrefixed is the point of the change: the vendor's
// path list is written with module prefixes, and every one of its 89 entries
// used to fail to resolve.
func TestResolveModelPathAcceptsPrefixed(t *testing.T) {
	cases := []struct{ in, want string }{{
		in:   "/org-openroadm-device:org-openroadm-device/info/clli",
		want: "/org-openroadm-device/info/clli",
	}, {
		in:   "/org-openroadm-device:org-openroadm-device/interface[name='ots-1/0/0/E1']/administrative-state",
		want: "/org-openroadm-device/interface[name='ots-1/0/0/E1']/administrative-state",
	}, {
		in:   "/org-openroadm-device:org-openroadm-device/interface[name='nmc-1']/org-openroadm-network-media-channel-interfaces:nmc-ctp/frequency",
		want: "/org-openroadm-device/interface[name='nmc-1']/nmc-ctp/frequency",
	}}

	for _, tc := range cases {
		resolved, found := ResolveModelPath(tc.in, openroadmRWPaths, openroadmROPaths)
		assert.True(t, found, "ResolveModelPath(%q) 應該找到", tc.in)
		assert.Equal(t, tc.want, resolved)
	}
}

// TestResolveModelPathKeepsBarePaths guards against a regression: paths that
// already worked must resolve to themselves, untouched.
func TestResolveModelPathKeepsBarePaths(t *testing.T) {
	bare := []string{
		"/org-openroadm-device/info/clli",
		"/org-openroadm-device/interface[name=ots-1/0/0/E1]/type",
		"/org-openroadm-device/interface", // container
		"/org-openroadm-device",           // top-level container
		"/active-alarm-list",              // read-only subtree
		"/active-alarm-list/activeAlarms/severity",
		"/current-pm-list/current-pm-entry/pm-resource-type",
		"/org-openroadm-device/info/current-datetime", // read-only leaf
		"/org-openroadm-device/circuit-packs[circuit-pack-name=1/0/0]/ports[port-name=E1]/port-name",
	}
	for _, p := range bare {
		resolved, found := ResolveModelPath(p, openroadmRWPaths, openroadmROPaths)
		assert.True(t, found, "ResolveModelPath(%q) 應該找到", p)
		assert.Equal(t, p, resolved, "合法路徑不該被改寫")
	}
}

// TestResolveModelPathRejectsUnknown is what lets Get answer NotFound instead
// of an empty value.
func TestResolveModelPathRejectsUnknown(t *testing.T) {
	unknown := []string{
		"/org-openroadm-device/info/no-such-leaf",
		"/not-a-module/interface/name",
		"/org-openroadm-device/interface[name=x]/no-such-leaf",
		// Prefix of a real path but not on a segment boundary: must not
		// match /org-openroadm-device/info-extra/clli-label.
		"/org-openroadm-device/info-ext",
	}
	for _, p := range unknown {
		_, found := ResolveModelPath(p, openroadmRWPaths, openroadmROPaths)
		assert.False(t, found, "ResolveModelPath(%q) 不該找到", p)
	}
}

// TestFindPathFromModelAcceptsPrefixed covers the Set side, which shares the
// same lookup.
func TestFindPathFromModelAcceptsPrefixed(t *testing.T) {
	_, rwPath, err := FindPathFromModel(
		"/org-openroadm-device:org-openroadm-device/info/clli", openroadmRWPaths, true)
	assert.NoError(t, err, "帶前綴的路徑應該找到")
	assert.NotNil(t, rwPath)

	// The bare form must still work unchanged.
	_, rwPath, err = FindPathFromModel("/org-openroadm-device/info/clli", openroadmRWPaths, true)
	assert.NoError(t, err)
	assert.NotNil(t, rwPath)

	// And a genuinely absent path must still fail.
	_, _, err = FindPathFromModel("/org-openroadm-device/info/nope", openroadmRWPaths, true)
	assert.Error(t, err, "不存在的路徑仍該回錯誤")
}

func TestSplitOutsideBrackets(t *testing.T) {
	got := splitOutsideBrackets("org-openroadm-device/interface[name=ots-1/0/0/E1]/type")
	assert.Equal(t, []string{"org-openroadm-device", "interface[name=ots-1/0/0/E1]", "type"}, got)
}

// TestResolveModelPathWildcardSubPaths covers the shape the real model uses:
// read-only subpaths carry wildcard list keys, so the indices have to be
// removed from the joined path rather than the base alone.
func TestResolveModelPathWildcardSubPaths(t *testing.T) {
	roPaths := ReadOnlyPathMap{
		"/historical-pm-list": ReadOnlySubPathMap{
			"/historical-pm-entry[pm-resource-instance=*][pm-resource-type=*]/pm-resource-instance":                                                  admin.ReadOnlySubPath{ValueType: 4},
			"/historical-pm-entry[pm-resource-instance=*][pm-resource-type=*]/historical-pm[direction=*][type=*]/measurement[bin-number=*]/validity": admin.ReadOnlySubPath{ValueType: 4},
		},
	}

	// The vendor list writes these without any keys at all.
	for _, p := range []string{
		"/historical-pm-list",
		"/historical-pm-list/historical-pm-entry",
		"/historical-pm-list/historical-pm-entry/pm-resource-instance",
		"/historical-pm-list/historical-pm-entry/historical-pm/measurement/validity",
	} {
		_, found := ResolveModelPath(p, ReadWritePathMap{}, roPaths)
		assert.True(t, found, "ResolveModelPath(%q) 應該找到", p)
	}

	// And the module-prefixed form must resolve to the bare one.
	resolved, found := ResolveModelPath(
		"/org-openroadm-pm:historical-pm-list/historical-pm-entry/pm-resource-instance",
		ReadWritePathMap{}, roPaths)
	assert.True(t, found)
	assert.Equal(t, "/historical-pm-list/historical-pm-entry/pm-resource-instance", resolved)

	// A leaf that is not in the model must still be rejected.
	_, found = ResolveModelPath("/historical-pm-list/historical-pm-entry/no-such-leaf",
		ReadWritePathMap{}, roPaths)
	assert.False(t, found)
}
