// SPDX-FileCopyrightText: 2020-present Open Networking Foundation <info@opennetworking.org>
//
// SPDX-License-Identifier: Apache-2.0

package path

import (
	"fmt"
	"regexp"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/onosproject/onos-lib-go/pkg/errors"

	"github.com/onosproject/onos-api/go/onos/config/admin"
	configapi "github.com/onosproject/onos-api/go/onos/config/v2"
	"github.com/onosproject/onos-lib-go/pkg/logging"
)

var log = logging.GetLogger("utils", "path")

// MatchOnIndex - regexp to find indices in paths names
const MatchOnIndex = `(\[.*?]).*?`

// validPathRegexp - permissible values in paths
const validPathRegexp = `(/[a-zA-Z0-9:=\-\._[\]]+)+`

// IndexAllowedChars - regexp to restrict characters in index names
//
// Forward slash is allowed: OpenROADM names its hardware with slashes
// (circuit-pack-name "1/0/0", interface "ots-1/0/0/E1"), and rejecting them makes
// those subtrees unconfigurable through gNMI.
const IndexAllowedChars = `^([a-zA-Z0-9\*\-\._/])+$`

// ReadOnlySubPathMap abstracts the read only subpath
type ReadOnlySubPathMap map[string]admin.ReadOnlySubPath

// ReadOnlyPathMap abstracts the read only path
type ReadOnlyPathMap map[string]ReadOnlySubPathMap

var rOnIndex = regexp.MustCompile(MatchOnIndex)
var rIndexAllowedChars = regexp.MustCompile(IndexAllowedChars)

// JustPaths extracts keys from a read only path map
func (ro ReadOnlyPathMap) JustPaths() []string {
	keys := make([]string, 0)
	for k, subPaths := range ro {
		for k1 := range subPaths {
			if k1 == "/" {
				keys = append(keys, k)
			} else {
				keys = append(keys, k+k1)
			}
		}
	}
	return keys
}

// TypeForPath finds the type from the model for a particular path
func (ro ReadOnlyPathMap) TypeForPath(path string) (configapi.ValueType, error) {
	for k, subPaths := range ro {
		for k1, sp := range subPaths {
			if k1 == "/" {
				if k == path {
					return sp.ValueType, nil
				}
			} else {
				if k+k1 == path {
					return sp.ValueType, nil
				}
			}
		}
	}
	return configapi.ValueType_EMPTY, fmt.Errorf("path %s not found in RO paths of model", path)
}

// ReadWritePathMap is a map of ReadWrite paths their metadata
type ReadWritePathMap map[string]admin.ReadWritePath

// NamespaceMap is a map of namespace prefixes to full names
type NamespaceMap map[string]string

// RemovePathIndices removes the index value from a path to allow it to be compared to a model path
func RemovePathIndices(path string) string {
	indices := rOnIndex.FindAllStringSubmatch(path, -1)
	for _, i := range indices {
		path = strings.Replace(path, i[0], "", 1)
	}
	return path
}

// AnonymizePathIndices anonymizes index value in a path (replaces it with *)
func AnonymizePathIndices(path string) string {
	indices := rOnIndex.FindAllStringSubmatch(path, -1)
	for _, i := range indices {
		idxParts := strings.Split(i[0], "=")
		idxParts[len(idxParts)-1] = "*]"
		path = strings.Replace(path, i[0], strings.Join(idxParts, "="), 1)
	}
	return path
}

// CheckPathIndexIsValid - check that index values have only the specified chars
func CheckPathIndexIsValid(index string) error {
	if !rIndexAllowedChars.MatchString(index) {
		return errors.NewInvalid("index value '%s' does not match pattern '%s'", index, IndexAllowedChars)
	}
	return nil
}

// ExtractIndexNames - get an ordered array of index names and index values
func ExtractIndexNames(path string) ([]string, []string) {
	indexNames := make([]string, 0)
	indexValues := make([]string, 0)
	jsonMatches := rOnIndex.FindAllStringSubmatch(path, -1)
	for _, m := range jsonMatches {
		idxName := m[1][1:strings.LastIndex(m[1], "=")]
		indexNames = append(indexNames, idxName)
		idxValue := m[1][strings.LastIndex(m[1], "=")+1 : len(m[1])-1]
		indexValues = append(indexValues, idxValue)
	}
	return indexNames, indexValues
}

// FindPathFromModel locates a path in the model's read-write paths.
//
// A path carrying RFC 7951 module prefixes is retried without them, so the
// NETCONF-style paths a device vendor publishes can be used as written. The
// prefixed form is tried first, because prefixes disambiguate augments and
// dropping them unconditionally would lose that.
func FindPathFromModel(path string, rwPaths ReadWritePathMap, exact bool) (bool, *admin.ReadWritePath, error) {
	isExact, rwPath, err := findPathFromModel(path, rwPaths, exact)
	if err == nil {
		return isExact, rwPath, nil
	}
	if stripped := StripModulePrefixes(path); stripped != path {
		if isExact, rwPath, retryErr := findPathFromModel(stripped, rwPaths, exact); retryErr == nil {
			return isExact, rwPath, nil
		}
	}
	return isExact, rwPath, err
}

func findPathFromModel(path string, rwPaths ReadWritePathMap, exact bool) (bool, *admin.ReadWritePath, error) {
	searchPathNoIndices := RemovePathIndices(path)

	// try exact match first
	if rwPath, isExactMatch := rwPaths[AnonymizePathIndices(path)]; isExactMatch {
		return true, &rwPath, nil
	} else if exact {
		return false, nil,
			status.Errorf(codes.InvalidArgument, "unable to find exact match for RW model path %s. %d paths inspected",
				path, len(rwPaths))
	}

	if strings.HasSuffix(path, "]") { //Ends with index
		indices, _ := ExtractIndexNames(path)
		// Add on the last index
		searchPathNoIndices = fmt.Sprintf("%s/%s", searchPathNoIndices, indices[len(indices)-1])
	}

	// First search through the RW paths
	for modelPath, modelElem := range rwPaths {
		pathNoIndices := RemovePathIndices(modelPath)
		// Find a short path
		if exact && pathNoIndices == searchPathNoIndices {
			return false, &modelElem, nil
		} else if !exact && strings.HasPrefix(pathNoIndices, searchPathNoIndices) {
			return false, &modelElem, nil // returns the first thing it finds that matches the prefix
		}
	}

	return false, nil,
		errors.NewInvalid("unable to find RW model path %s ( without index %s). %d paths inspected", path, searchPathNoIndices, len(rwPaths))
}

// CheckKeyValue checks that if this is a Key attribute, that the value is the same as its parent's key
func CheckKeyValue(path string, rwPath *admin.ReadWritePath, val *configapi.TypedValue) error {
	indexNames, indexValues := ExtractIndexNames(path)
	if len(indexNames) == 0 {
		return nil
	}
	for i, idxName := range indexNames {
		if err := CheckPathIndexIsValid(indexValues[i]); err != nil {
			return err
		}
		if !rwPath.IsAKey || rwPath.AttrName == idxName && indexValues[i] == val.ValueToString() {
			return nil
		}
	}
	return errors.NewInvalid("index attribute %s=%s does not match %s", rwPath.AttrName, val.ValueToString(), path)
}

// IsPathValid tests for valid paths. Path is valid if it
// 1) starts with a slash
// 2) is followed by at least one of alphanumeric or any of : = - _ [ ]
// 3) and any further combinations of 1+2
// Two contiguous slashes are not allowed
// Paths not starting with slash are not allowed
func IsPathValid(path string) error {
	r1 := regexp.MustCompile(validPathRegexp)

	match := r1.FindString(path)
	if path != match {
		return errors.NewInvalid("invalid path %s. Must match %s", path, validPathRegexp)
	}
	return nil
}

// GetParentPath returns the immediate parent path of the specified path; empty string if "/" is given
func GetParentPath(path string) string {
	i := strings.LastIndex(path, "/")
	if i <= 0 {
		return ""
	}
	return path[0:i]
}

// StripModulePrefixes removes RFC 7951 module prefixes from a path's element
// names, leaving list key predicates untouched.
//
//	/org-openroadm-device:org-openroadm-device/info/clli
//	  -> /org-openroadm-device/info/clli
//
// Prefixes carry meaning — they disambiguate augments from different modules —
// so this is only ever a second attempt after the path as written has failed
// to resolve. Callers should use ResolveModelPath rather than calling this
// directly.
func StripModulePrefixes(path string) string {
	if !strings.Contains(path, ":") {
		return path
	}
	segments := splitOutsideBrackets(strings.TrimPrefix(path, "/"))
	for i, seg := range segments {
		name, pred := seg, ""
		if j := strings.IndexByte(seg, '['); j >= 0 {
			name, pred = seg[:j], seg[j:]
		}
		// A colon only introduces a module prefix in the element name.
		if k := strings.IndexByte(name, ':'); k >= 0 {
			name = name[k+1:]
		}
		segments[i] = name + pred
	}
	return "/" + strings.Join(segments, "/")
}

// splitOutsideBrackets splits on "/" except where it falls inside a list key
// predicate.
//
// OpenROADM list keys contain slashes routinely — interface names look like
// "ots-1/0/0/E1" — so splitting the whole string would cut a key into pieces.
func splitOutsideBrackets(s string) []string {
	var out []string
	depth, start := 0, 0
	for i, r := range s {
		switch r {
		case '[':
			depth++
		case ']':
			if depth > 0 {
				depth--
			}
		case '/':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// ResolveModelPath checks whether a path exists in the model and returns the
// form that matches.
//
// A path is considered present when it names a leaf, a container on the way to
// one, or a read-only subtree. The path as written is tried first; only if
// that fails is it retried with module prefixes stripped, which lets a caller
// use the NETCONF-style paths a device vendor publishes
// (org-openroadm-device:org-openroadm-device/...) without having to translate
// them by hand.
//
// found is false only when neither form appears anywhere in the model. That
// distinction is what lets Get answer NotFound for a path that cannot exist,
// instead of an empty value that looks like "the device has nothing here".
func ResolveModelPath(path string, rwPaths ReadWritePathMap, roPaths ReadOnlyPathMap) (resolved string, found bool) {
	if pathInModel(path, rwPaths, roPaths) {
		return path, true
	}
	if stripped := StripModulePrefixes(path); stripped != path {
		if pathInModel(stripped, rwPaths, roPaths) {
			return stripped, true
		}
	}
	return path, false
}

// pathInModel reports whether a path names or leads to a modelled node.
func pathInModel(path string, rwPaths ReadWritePathMap, roPaths ReadOnlyPathMap) bool {
	anonymized := AnonymizePathIndices(path)
	if _, ok := rwPaths[anonymized]; ok {
		return true
	}

	// The root is always addressable; so is a path ending in a list key,
	// whose anonymized form loses the trailing predicate.
	if path == "" || path == "/" {
		return true
	}

	noIndices := RemovePathIndices(path)
	for modelPath := range rwPaths {
		if matchesModelPath(noIndices, anonymized, RemovePathIndices(modelPath)) {
			return true
		}
	}
	// Read-only subpaths carry wildcard list keys, e.g.
	//   base "/historical-pm-list"
	//   sub  "/historical-pm-entry[pm-resource-type=*][...]/pm-resource-instance"
	// so the indices have to come off the joined path, not just the base.
	for roPath, subPaths := range roPaths {
		base := RemovePathIndices(roPath)
		if matchesModelPath(noIndices, anonymized, base) {
			return true
		}
		for sub := range subPaths {
			if sub == "/" {
				continue
			}
			if matchesModelPath(noIndices, anonymized, RemovePathIndices(roPath+sub)) {
				return true
			}
		}
	}
	return false
}

// matchesModelPath reports whether a requested path addresses a model path,
// either exactly or as one of its ancestors.
//
// The ancestor case matters because Get is usually asked for a container:
// /org-openroadm-device/interface is a legitimate query even though only the
// leaves beneath it appear in the model maps. The boundary check prevents
// /a/foo from matching the model path /a/foobar.
func matchesModelPath(requested, anonymized, modelPath string) bool {
	for _, candidate := range []string{requested, anonymized} {
		if candidate == modelPath {
			return true
		}
		if strings.HasPrefix(modelPath, candidate) && len(modelPath) > len(candidate) {
			switch modelPath[len(candidate)] {
			case '/', '[':
				return true
			}
		}
	}
	return false
}
