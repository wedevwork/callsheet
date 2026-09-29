//go:build darwin

package function

import (
	"os"
	"testing"
)

// pipeCapacity is the most a Darwin pipe buffers: 64 KiB (BIG_PIPE_SIZE;
// Darwin grows a pipe from 16 KiB up to that bound and offers no query).
func pipeCapacity(t *testing.T, f *os.File) int { return 64 << 10 }
