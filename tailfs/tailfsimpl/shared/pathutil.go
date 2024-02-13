// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package shared

import (
	"path"
	"strings"
)

// This file provides utility functions for working with URL paths. These are
// similar to functions in package path in the standard library, but differ in
// ways that are documented on the relevant functions.

const (
	sepString       = "/"
	sepStringAndDot = "/."
	sep             = '/'
)

// CleanAndSplit cleans the provided path p and splits it into its constituent
// parts. This is different from path.Split which just splits a path into prefix
// and suffix.
func CleanAndSplit(p string) []string {
	return strings.Split(strings.Trim(path.Clean(p), sepStringAndDot), sepString)
}

// Join behaves like path.Join() but also includes a leading slash.
func Join(parts ...string) string {
	fullParts := make([]string, 0, len(parts))
	fullParts = append(fullParts, sepString)
	for _, part := range parts {
		fullParts = append(fullParts, part)
	}
	return path.Join(fullParts...)
}

// IsRoot determines whether a given path p is the root path, defined as either
// empty or "/".
func IsRoot(p string) bool {
	return p == "" || p == sepString
}
