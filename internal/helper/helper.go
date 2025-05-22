// Copyright 2018 Google LLC All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package helper

import (
	"net/http"
	"strings"
)

// Returns whether this url should be handled by the Blob handler
// This is complicated because Blob is indicated by the trailing path, not the l
eading path.
// https://github.com/opencontainers/distribution-spec/blob/master/spec.md#pulli
ng-a-layer
// https://github.com/opencontainers/distribution-spec/blob/master/spec.md#pushi
ng-a-layer
func IsBlob(req *http.Request) bool {
	elem := strings.Split(req.URL.Path, "/")
	elem = elem[1:]
	if elem[len(elem)-1] == "" {
		elem = elem[:len(elem)-1]
	}
	if len(elem) < 3 {
		return false
	}
	return elem[len(elem)-2] == "blobs" || (elem[len(elem)-3] == "blobs" &&
		elem[len(elem)-2] == "uploads")
}

func IsManifest(req *http.Request) bool {
	elems := strings.Split(req.URL.Path, "/")
	elems = elems[1:]
	if len(elems) < 4 {
		return false
	}
	return elems[len(elems)-2] == "manifests"
}

func IsTags(req *http.Request) bool {
	elems := strings.Split(req.URL.Path, "/")
	elems = elems[1:]
	if len(elems) < 4 {
		return false
	}
	return elems[len(elems)-2] == "tags"
}

func IsCatalog(req *http.Request) bool {
	elems := strings.Split(req.URL.Path, "/")
	elems = elems[1:]
	if len(elems) < 2 {
		return false
	}

	return elems[len(elems)-1] == "_catalog"
}

func IsV2(req *http.Request) bool {
	elems := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
	if len(elems) < 1 {
		return false
	}
	return elems[len(elems)-1] == "v2"
}

// SemVerReplace replaces all underscores in a given string with plus signs.
// This function is primarily intended for use with semantic version strings
// where underscores might have been used in place of plus signs (e.g., for
// build metadata or pre-release identifiers). Standard Semantic Versioning
// (semver.org) specifies plus signs for build metadata (e.g., "1.0.0+20130313144700")
// and hyphens for pre-release identifiers (e.g., "1.0.0-alpha").
//
// Purpose:
// The main purpose of this function is to normalize version strings by converting
// any underscores to plus signs. This can be particularly useful when dealing with
// version strings from systems or sources that use underscores due to constraints
// (e.g., where '+' is a special character) or by convention for information that
// semantically aligns with build metadata.
//
// When to use it:
// Use this function when you encounter version strings like "v1.2.3_build456" or
// "2.0.0_rc_1" and need to transform them into a format like "v1.2.3+build456" or
// "2.0.0+rc+1". This transformation is often a preparatory step before parsing
// the string with a semantic versioning library that strictly expects '+' for
// build metadata, or when aiming for a consistent display format for version information.
//
// Transformation Examples:
//   - Input: "1.0.0_alpha"
//     Output: "1.0.0+alpha"
//   - Input: "v2.1.3_beta_build123" (handles multiple underscores)
//     Output: "v2.1.3+beta+build123"
//   - Input: "1.2.3" (string with no underscores)
//     Output: "1.2.3" (string remains unchanged)
//   - Input: "" (empty string)
//     Output: "" (empty string remains unchanged)
//
// Semver Validation:
// This function does NOT perform validation of the overall semantic version string structure.
// For example, it does not check if the version string conforms to the MAJOR.MINOR.PATCH
// numerical format or other specific semver rules. Its sole responsibility is to
// replace every occurrence of the underscore character '_' with a plus sign '+'.
// For comprehensive semver parsing and validation, it is recommended to use a
// dedicated semver library on the string after this transformation, if necessary.
//
// Edge Cases Handled:
// - Multiple underscores: All occurrences of underscores are replaced.
//   For instance, "1.0.0_alpha_snapshot" becomes "1.0.0+alpha+snapshot".
// - Empty string: If an empty string is provided, an empty string is returned.
// - String without underscores: If the string does not contain any underscores,
//   it is returned as is.
func SemVerReplace(semver string) string {
	// strings.ReplaceAll is efficient and handles edge cases gracefully:
	// - If `semver` is an empty string, it returns an empty string.
	// - If `semver` does not contain "_", it returns `semver` unchanged.
	// - It replaces all occurrences of "_" with "+".
	// Therefore, the original conditional check (if semver != "" && strings.Contains(semver, "_"))
	// is not strictly necessary for correctness.
	return strings.ReplaceAll(semver, "_", "+")
}
