package audio

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"sync"
)

// mixLag is how many frames a source may queue while another has none
// before its frames go out unmixed. Capture callbacks arrive in bursts of up
// to about 100 ms and the sources' clocks drift apart, so frames rarely
// arrive in step; a source that stalls delays the others by at most this.
const mixLag = 3

// Mix returns a Source that sums its sources, such as what the Mac plays
// and the microphone. It starts them in order, so their permission prompts
// come one at a time. A source that finds no microphone (ErrNoMicrophone)
// is left out while another one starts; any other failure stops those
// already started.
func Mix(srcs ...Source) Source { return &mix{srcs: srcs} }

type mix struct {
	srcs []Source
}

// Err implements Source: the first source error.
func (m *mix) Err() error {
	for _, s := range m.srcs {
		if err := s.Err(); err != nil {
			return err
		}
	}
	return nil
}

// Start implements Source.
func (m *mix) Start(ctx context.Context) (<-chan []byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	var chans []<-chan []byte
	var missing error
	for _, s := range m.srcs {
		ch, err := s.Start(ctx)
		if errors.Is(err, ErrNoMicrophone) {
			missing = err
			continue
		}
		if err != nil {
			cancel()
			return nil, err
		}
		chans = append(chans, ch)
	}
	if len(chans) == 0 {
		cancel()
		return nil, missing
	}

	type arrival struct {
		src   int
		pcm   []byte
		ended bool
	}
	in := make(chan arrival)
	var wg sync.WaitGroup
	for i, ch := range chans {
		wg.Go(func() {
			for pcm := range ch {
				in <- arrival{src: i, pcm: pcm}
			}
			in <- arrival{src: i, ended: true}
		})
	}
	go func() {
		wg.Wait()
		close(in)
	}()

	// Like a sink, the mix never waits for the consumer: frames beyond the
	// buffer are dropped.
	out := make(chan []byte, sinkBuffer)
	go func() {
		defer cancel()
		defer close(out)
		mx := newMixer(len(chans))
		for a := range in {
			if a.ended {
				mx.ended[a.src] = true
			} else {
				mx.queues[a.src] = append(mx.queues[a.src], a.pcm)
			}
			for _, frame := range mx.ready() {
				select {
				case out <- frame:
				default:
				}
			}
		}
	}()
	return out, nil
}

// mixer lines up the frames of several sources in arrival order.
type mixer struct {
	queues [][][]byte
	ended  []bool
}

func newMixer(n int) *mixer {
	return &mixer{queues: make([][][]byte, n), ended: make([]bool, n)}
}

// ready returns the frames that can go out: the sources' oldest frames
// summed once every running source has one, or sooner when a source runs
// more than mixLag frames ahead of one that has none.
func (m *mixer) ready() [][]byte {
	var out [][]byte
	for {
		var heads [][]byte
		waiting, ahead := false, false
		for i, q := range m.queues {
			switch {
			case len(q) > 0:
				heads = append(heads, q[0])
				ahead = ahead || len(q) > mixLag
			case !m.ended[i]:
				waiting = true
			}
		}
		if len(heads) == 0 || waiting && !ahead {
			return out
		}
		out = append(out, sumPCM(heads))
		for i, q := range m.queues {
			if len(q) > 0 {
				m.queues[i] = q[1:]
			}
		}
	}
}

// sumPCM adds PCM16 frames sample by sample, clipping at full scale. The
// result is as long as the longest frame.
func sumPCM(frames [][]byte) []byte {
	if len(frames) == 1 {
		return frames[0]
	}
	n := 0
	for _, f := range frames {
		n = max(n, len(f))
	}
	acc := make([]int32, n/2)
	for _, f := range frames {
		for i := 0; i+1 < len(f); i += 2 {
			acc[i/2] += int32(int16(binary.LittleEndian.Uint16(f[i:])))
		}
	}
	out := make([]byte, n)
	for i, v := range acc {
		v = max(min(v, math.MaxInt16), math.MinInt16)
		binary.LittleEndian.PutUint16(out[i*2:], uint16(int16(v)))
	}
	return out
}
