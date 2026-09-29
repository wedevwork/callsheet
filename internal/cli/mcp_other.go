//go:build !linux && !darwin

package cli

import "io"

// mcpStreams owns only closable test streams outside the supported
// systems; the mcp leaf rejects them before serving.
func mcpStreams(in io.Reader, out, errOut io.Writer) mcpIO { return ownGeneric(in, out) }
