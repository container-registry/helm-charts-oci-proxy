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

import "strings"

// SplitPath splits /v2/<repo>/<kind>/<ref>. Routing and the manifest, tag and
// blob handlers share it, so a blob is looked up under the same repository
// key as its manifest.
func SplitPath(p string) (repo, kind, ref string, ok bool) {
	rest, found := strings.CutPrefix(p, "/v2/")
	i := strings.LastIndexByte(rest, '/')
	if !found || i < 0 {
		return "", "", "", false
	}
	rest, ref = rest[:i], rest[i+1:]
	i = strings.LastIndexByte(rest, '/')
	if i < 0 {
		return "", "", "", false
	}
	return rest[:i], rest[i+1:], ref, true
}

// Helm writes build metadata into OCI tags with "_" in place of "+", because the
// distribution spec's tag grammar has no "+" (helm/helm#10166). Semver forbids
// "_", so the mapping is lossless in both directions.
func TagToVersion(tag string) string {
	return strings.ReplaceAll(tag, "_", "+")
}

func VersionToTag(version string) string {
	return strings.ReplaceAll(version, "+", "_")
}
