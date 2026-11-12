// Small helpers split into a separate file so tools.go reads as the
// tool catalogue, not as an io wrapper.
package mcp

import (
	"bytes"
	"io"
)

func bytesReader(b []byte) io.Reader        { return bytes.NewReader(b) }
func ioReadAll(r io.Reader) ([]byte, error) { return io.ReadAll(io.LimitReader(r, 8<<20)) }
