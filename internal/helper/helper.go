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

// SplitPath splits [<prefix>/]v2/<repo>/<kind>/<ref>. Harbor's proxy cache puts
// its own path in front of /v2/, so the repository is what lies between the
// last "v2" segment and kind; without one, the first segment is dropped. The
// manifest, tag and blob handlers share it, so a blob is looked up under the
// same repository key as its manifest.
func SplitPath(p string) (repo, kind, ref string, ok bool) {
	elem := strings.Split(p, "/")
	n := len(elem)
	if n < 5 {
		return "", "", "", false
	}
	start := n - 2
	for start > 2 && elem[start-1] != "v2" {
		start--
	}
	return strings.Join(elem[start:n-2], "/"), elem[n-2], elem[n-1], true
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
