package helper

import (
	"testing"
)

func TestSemVerReplace(t *testing.T) {
	testCases := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "no underscores",
			input:    "1.0.0",
			expected: "1.0.0",
		},
		{
			name:     "one underscore",
			input:    "1.0.0_alpha",
			expected: "1.0.0+alpha",
		},
		{
			name:     "multiple underscores",
			input:    "1.0.0_alpha_build123",
			expected: "1.0.0+alpha+build123",
		},
		{
			name:     "leading and trailing underscores",
			input:    "_1.0.0_",
			expected: "+1.0.0+",
		},
		{
			name:     "only underscores",
			input:    "___",
			expected: "+++",
		},
		{
			name:     "mixed case and underscores",
			input:    "v1.2.3_Rc1_candidate",
			expected: "v1.2.3+Rc1+candidate",
		},
		{
			name:     "no underscores with v prefix",
			input:    "v2.3.4",
			expected: "v2.3.4",
		},
		{
			name:     "already has plus (should not change)",
			input:    "1.0.0+beta",
			expected: "1.0.0+beta",
		},
		{
			name:     "mixed plus and underscore",
			input:    "1.0.0_alpha+beta_rc1",
			expected: "1.0.0+alpha+beta+rc1",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			actual := SemVerReplace(tc.input)
			if actual != tc.expected {
				t.Errorf("SemVerReplace(%q) = %q; want %q", tc.input, actual, tc.expected)
			}
		})
	}
}
