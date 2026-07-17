package main

import "testing"

func TestHasUnsupportedFindFlags(t *testing.T) {
	tests := []struct {
		cmd      string
		expected bool
	}{
		// Simple supported predicates — should be false
		{"find . -name '*.go'", false},
		{"find . -type f -name '*.rs'", false},
		{"find src -name '*.toml' -maxdepth 2", false},
		{"find . -type d", false},
		{"find . -iname 'Makefile'", false},
		{"find /tmp -name '*.txt' -type f -maxdepth 1", false},

		// Compound predicates — should be true
		{"find . -not -path '*/.git/*' -name '*.go'", true},
		{"find . ! -name '*.go'", true},
		{"find . \\( -name '*.go' -o -name '*.rs' \\)", true},
		{"find . -type f -exec wc -l {} +", true},
		{"find . -type f -size +1M", true},
		{"find . -type f -delete", true},
		{"find . -empty", true},
		{"find . -perm 644", true},
		{"find . -newer file.txt", true},
		{"find . -mtime -7", true},
		{"find . -regex '.*\\.go'", true},
		{"find . -type f -print0", true},
		{"find . -mindepth 2 -type f", true},

		// Flags inside quoted strings — should not trigger
		{"find . -name '-not-a-flag.txt'", false},
		{"find . -name '*-size*'", false},
		{"find . -name \"*!*\"", false},

		// Not a find command — function still detects flags (caller filters by cmd)
		{"ls -not", true},
		{"git exec", false},

		// Double-dash end-of-flags
		{"find . -type f -- -name 'foo.go'", false},

		// Supported: -L is silently ignored by rtk, treat as unsupported
		{"find -L . -name '*.go'", true},
		{"find . -follow -name '*.go'", true},

		// Pseudo-flag in path or pattern, not a real flag
		{"find . -path './-not-a-flag.go'", false},
		{"find . -name '*-size*'", false},
		{"find . -name \"*!*\"", false},

		// Edge: \\( ... \\) are shell-escaped parens, not flags
		{"find . \\( -name '*.go' -o -name '*.rs' \\)", true}, // -o is unsupported
		{"find . \\( -name '*.go' -or -name '*.rs' \\)", true}, // -or is unsupported

		// Chain operators are handled by splitChain before this runs
		{"find . -name '*.go'", false},
		{"find . -size +1M", true},
		{"find . -type f -newer file.txt -name '*.go'", true},

		// Unsupported actions
		{"find . -name '*.go' -ls", true},
		{"find . -name '*.go' -prune", true},

		// -regex variants
		{"find . -regex '.*\\.go'", true},
		{"find . -iregex '.*\\.GO'", true},
	}

	for _, tc := range tests {
		t.Run("", func(t *testing.T) {
			got := hasUnsupportedFindFlags(tc.cmd)
			if got != tc.expected {
				t.Errorf("hasUnsupportedFindFlags(%q) = %v, want %v", tc.cmd, got, tc.expected)
			}
		})
	}
}

func TestRewriteChain_FindUnsupported(t *testing.T) {
	tests := []struct {
		input    string
		expected string // the entire rewritten command; empty means unchanged
	}{
		// Simple find — gets rtk prefix
		{
			input:    "find . -name '*.go'",
			expected: "rtk find . -name '*.go'",
		},
		{
			input:    "find src -type f -name '*.rs'",
			expected: "rtk find src -type f -name '*.rs'",
		},

		// Compound find — stays unchanged (no rtk)
		{
			input:    "find . -not -path '*/.git/*' -name '*.go'",
			expected: "find . -not -path '*/.git/*' -name '*.go'",
		},
		{
			input:    "find . -type f -size +1M",
			expected: "find . -type f -size +1M",
		},
		{
			input:    "find . -type f -exec wc -l {} +",
			expected: "find . -type f -exec wc -l {} +",
		},

		// Other commands unaffected
		{
			input:    "ls -la",
			expected: "rtk ls -la",
		},
		{
			input:    "grep -r 'pattern' src/",
			expected: "rtk grep -r 'pattern' src/",
		},

		// Chain with simple + compound
		{
			input:    "find . -name '*.go' && find . -type f -size +1M",
			expected: "rtk find . -name '*.go' && find . -type f -size +1M",
		},

		// Already prefixed
		{
			input:    "rtk find . -not -path '*/.git/*'",
			expected: "rtk find . -not -path '*/.git/*'",
		},
	}

	for _, tc := range tests {
		t.Run("", func(t *testing.T) {
			got := rewriteChain(tc.input)
			if got != tc.expected {
				t.Errorf("rewriteChain(%q)\n  got:  %q\n  want: %q", tc.input, got, tc.expected)
			}
		})
	}
}
