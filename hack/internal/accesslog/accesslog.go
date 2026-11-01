// Package accesslog reads ingress-nginx access lines from a VictoriaLogs JSONL
// export. replay and upstream-mirror both use it, so the mirror records exactly
// the requests the replay sends.
package accesslog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"time"
)

type Request struct {
	At     time.Time
	Method string
	Path   string
}

var (
	accessLine = regexp.MustCompile(`^\S+ - \S+ \[[^\]]+\] "(GET|HEAD) (\S+) `)
	registryOp = regexp.MustCompile(`/(manifests|tags/list|blobs)/?`)
)

// ParseWindow parses RFC 3339 window bounds such as 2026-11-01T07:59:16Z.
func ParseWindow(from, to string) (time.Time, time.Time, error) {
	f, err := time.Parse(time.RFC3339, from)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("-from: %w", err)
	}
	t, err := time.Parse(time.RFC3339, to)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("-to: %w", err)
	}
	if t.Before(f) {
		return time.Time{}, time.Time{}, fmt.Errorf("-to %s is before -from %s", to, from)
	}
	return f, t, nil
}

// Read returns the registry GET and HEAD requests logged between from and to
// (inclusive), sorted by time.
func Read(path string, from, to time.Time) ([]Request, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Request
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var rec struct {
			Time string `json:"_time"`
			Msg  string `json:"_msg"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, rec.Time)
		if err != nil || at.Before(from) || at.After(to) {
			continue
		}
		m := accessLine.FindStringSubmatch(rec.Msg)
		if m == nil || !registryOp.MatchString(m[2]) {
			continue
		}
		out = append(out, Request{At: at, Method: m[1], Path: m[2]})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, sc.Err()
}
