// Package features says which of Capy's optional features are on.
package features

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
)

// Feature names an optional feature, as features.json does.
type Feature string

// The features of this build.
const (
	// Agent is Agent: Capy's recordings with the user's own agents, Ask
	// among them.
	Agent Feature = "agent"
	// Highlights marks the turns that matter while recording.
	Highlights Feature = "highlights"
)

// Gate says whether a feature is on. It is asked every time a feature
// would show or run, so a gate whose answer changes takes effect at once.
type Gate interface {
	Enabled(Feature) bool
}

// defaults are the features of this build and whether each is on when
// nothing overrides it.
var defaults = map[Feature]bool{Agent: true, Highlights: true}

// local is a Gate of the defaults with the overrides read from a file.
type local map[Feature]bool

// Enabled implements Gate. A feature this build lacks is off.
func (g local) Enabled(f Feature) bool { return g[f] }

// ErrMalformed is Local's error for a file that is not an object of
// booleans. It never holds the file's content.
var ErrMalformed = errors.New("features: malformed file, using the defaults")

// Local is the built-in defaults overridden by the file at path, such as
// {"agent": false}, read once. Keys this build does not know are ignored:
// the file shows or hides only features the build has. A missing file
// gives the defaults; an unreadable or malformed one gives them too, with
// the error to log.
func Local(path string) (Gate, error) {
	g := local(maps.Clone(defaults))
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return g, nil
	}
	if err != nil {
		// The cause, not the path's content; the path is Capy's own.
		if pe, ok := errors.AsType[*fs.PathError](err); ok {
			err = pe.Err
		}
		return g, fmt.Errorf("features: read: %w", err)
	}
	var overrides map[Feature]bool
	if json.Unmarshal(b, &overrides) != nil || overrides == nil {
		return g, ErrMalformed
	}
	for f, on := range overrides {
		if _, ok := defaults[f]; ok {
			g[f] = on
		}
	}
	return g, nil
}
