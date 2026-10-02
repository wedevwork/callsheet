//go:build !linux && !darwin

package adapter

import (
	"errors"
	"io"
	"os"
	"time"
)

// probePipes are unsupported outside Linux and macOS: no probe child is
// started there, and the CLI's platform rejection keeps this unreachable.
type probePipes struct{}

func openProbePipes(io.Writer, io.Writer) (*probePipes, *os.File, *os.File, error) {
	return nil, nil, nil, errors.New("adapter: probe pipes are not supported on this system")
}

func (*probePipes) abandon()                          {}
func (*probePipes) drain()                            {}
func (*probePipes) settle(time.Duration) settleResult { return settleFailed }
