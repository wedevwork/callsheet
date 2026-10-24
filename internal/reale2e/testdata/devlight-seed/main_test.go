package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	cases := []struct {
		id     string
		args   []string
		code   int
		stdout string
		stderr bool
	}{
		{"no_args", nil, 0, "Hello, world!\n", false},
		{"name", []string{"--name", "Ada"}, 0, "Hello, Ada!\n", false},
		{"spaces", []string{"--name", "Ada Lovelace"}, 0, "Hello, Ada Lovelace!\n", false},
		{"unicode", []string{"--name", "世界"}, 0, "Hello, 世界!\n", false},
		{"empty_name", []string{"--name", ""}, 2, "", true},
		{"unknown_flag", []string{"--unknown"}, 2, "", true},
		{"missing_value", []string{"--name"}, 2, "", true},
		{"positional", []string{"Ada"}, 2, "", true},
		{"help", []string{"--help"}, 0, "", true},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := run(c.args, &stdout, &stderr); got != c.code {
				t.Fatalf("run(%q) = %d, want %d", c.args, got, c.code)
			}
			if stdout.String() != c.stdout || (stderr.Len() > 0) != c.stderr {
				t.Fatalf("run(%q) wrote %q and %q", c.args, stdout.String(), stderr.String())
			}
			if c.id == "help" && !strings.Contains(stderr.String(), "name") {
				t.Fatalf("usage %q does not mention name", stderr.String())
			}
		})
	}
}
