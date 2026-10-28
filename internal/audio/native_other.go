//go:build !darwin

package audio

import "context"

// Mic is only available on macOS.
type Mic struct{}

// NewMic returns a Source that fails with ErrUnsupported.
func NewMic() *Mic { return &Mic{} }

// Start implements Source.
func (*Mic) Start(context.Context) (<-chan []byte, error) { return nil, ErrUnsupported }

// Err implements Source.
func (*Mic) Err() error { return nil }

// System is only available on macOS.
type System struct{}

// NewSystem returns a Source that fails with ErrUnsupported.
func NewSystem() *System { return &System{} }

// Start implements Source.
func (*System) Start(context.Context) (<-chan []byte, error) { return nil, ErrUnsupported }

// Err implements Source.
func (*System) Err() error { return nil }
