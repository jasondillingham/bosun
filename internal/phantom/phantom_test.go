package phantom

import "testing"

func TestIsLikelyPhantom(t *testing.T) {
	cases := []struct {
		name string
		exts []string
		want bool
	}{
		// Spotlight shape with the json allow-list (claims).
		{"session-1.json", []string{"json"}, false},
		{"session-1 2.json", []string{"json"}, true},
		{"session-1 99.json", []string{"json"}, true},

		// iCloud shape with the json allow-list.
		{"session-1 (1).json", []string{"json"}, true},
		{"session-1 (12).json", []string{"json"}, true},

		// Multi-extension allow-list (state markers).
		{"session-1 2.done", []string{"done", "stuck", "heartbeat", "json"}, true},
		{"session-1 2.stuck", []string{"done", "stuck", "heartbeat", "json"}, true},
		{"session-1 2.heartbeat", []string{"done", "stuck", "heartbeat", "json"}, true},
		{"session-1 2.txt", []string{"done", "stuck", "heartbeat", "json"}, false}, // ext not in list

		// No allow-list: any extension is fair game.
		{"section 2.txt", nil, true},
		{"section 2.go", nil, true},

		// Negative cases.
		{"session.json", []string{"json"}, false},
		{"session-1.done", []string{"done"}, false},
		{".lock", []string{"json"}, false},
		{"", []string{"json"}, false},
		{"session-1 abc.json", []string{"json"}, false}, // non-digit middle
		{"session-1 2", []string{"json"}, false},        // no extension

		// Empty allow-list (variadic with zero args) behaves like nil.
		{"session 2.json", []string{}, true},
	}
	for _, tc := range cases {
		if got := IsLikelyPhantom(tc.name, tc.exts...); got != tc.want {
			t.Errorf("IsLikelyPhantom(%q, %v) = %v, want %v", tc.name, tc.exts, got, tc.want)
		}
	}
}
