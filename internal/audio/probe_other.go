//go:build !darwin

package audio

// Probe is only available on macOS: elsewhere it fails with ErrUnsupported.
func Probe(string) (Info, error) { return Info{}, ErrUnsupported }
