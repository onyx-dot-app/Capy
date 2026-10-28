package audio

import (
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego/objc"
)

// resampler converts captured AVAudioPCMBuffers to 16 kHz mono PCM16 with
// AVAudioConverter and writes them to a sink. Its convert methods run on
// the capture's audio thread.
type resampler struct {
	from, target, converter objc.ID
	rate                    float64
	provide                 objc.Block
	pending                 objc.ID // the buffer provide hands to the converter
	sink                    *sink
	what                    string // the audio, for errors: "microphone audio"

	// mu keeps close from freeing the converter while a conversion, on the
	// audio thread, still uses it; stopped turns away later callbacks.
	mu      sync.Mutex
	stopped bool
}

// newResampler converts from, an AVAudioFormat it retains, into s.
func newResampler(from objc.ID, s *sink, what string) (*resampler, error) {
	r := &resampler{from: send(from, "retain"), rate: objc.Send[float64](from, sel("sampleRate")), sink: s, what: what}
	formatClass, _ := class("AVAudioFormat")
	r.target = send(send(formatClass, "alloc"), "initWithCommonFormat:sampleRate:channels:interleaved:",
		uint(avAudioPCMFormatInt16), float64(SampleRate), uint32(1), true)
	converterClass, _ := class("AVAudioConverter")
	r.converter = send(send(converterClass, "alloc"), "initFromFormat:toFormat:", from, r.target)
	if r.converter == 0 {
		r.release()
		return nil, fmt.Errorf("audio: cannot convert %.0f Hz %s", r.rate, what)
	}
	// The converter pulls input through provide, once per captured buffer.
	r.provide = objc.NewBlock(func(_ objc.Block, _ uint32, status *int) objc.ID {
		if r.pending == 0 {
			*status = avConverterInputNoDataNow
			return 0
		}
		buf := r.pending
		r.pending = 0
		*status = avConverterInputHaveData
		return buf
	})
	return r, nil
}

// convert converts buf, an AVAudioPCMBuffer in the source format.
func (r *resampler) convert(buf objc.ID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return
	}
	pool := poolPush()
	defer poolPop(pool)
	r.convertLocked(buf)
}

// convertList converts the samples of abl, an AudioBufferList in the
// source format, without copying them.
func (r *resampler) convertList(abl unsafe.Pointer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return
	}
	pool := poolPush()
	defer poolPop(pool)
	bufferClass, _ := class("AVAudioPCMBuffer")
	buf := send(send(bufferClass, "alloc"), "initWithPCMFormat:bufferListNoCopy:deallocator:", r.from, abl, uintptr(0))
	if buf == 0 {
		return // an empty or malformed buffer list
	}
	defer send(buf, "release")
	r.convertLocked(buf)
}

func (r *resampler) convertLocked(buf objc.ID) {
	frames := objc.Send[uint32](buf, sel("frameLength"))
	capacity := uint32(float64(frames)*SampleRate/r.rate) + 64
	bufferClass, _ := class("AVAudioPCMBuffer")
	out := send(send(bufferClass, "alloc"), "initWithPCMFormat:frameCapacity:", r.target, capacity)
	defer send(out, "release")

	r.pending = buf
	errp := new(objc.ID)
	status := objc.Send[int](r.converter, sel("convertToBuffer:error:withInputFromBlock:"), out, unsafe.Pointer(errp), r.provide)
	runtime.KeepAlive(errp)
	if status == avConverterOutputError {
		r.sink.fail(fmt.Errorf("audio: convert %s: %w", r.what, nsError(*errp)))
		return
	}
	n := objc.Send[uint32](out, sel("frameLength"))
	if n == 0 {
		return
	}
	channels := objc.Send[**int16](out, sel("int16ChannelData"))
	r.sink.write(unsafe.Slice((*byte)(unsafe.Pointer(*channels)), int(n)*2))
}

// close turns away later buffers and frees the converter. Stop the
// capture's callbacks first: one in progress holds the lock until it
// returns.
func (r *resampler) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopped = true
	r.release()
}

func (r *resampler) release() {
	for _, obj := range []objc.ID{r.converter, r.target, r.from} {
		if obj != 0 {
			send(obj, "release")
		}
	}
	r.converter, r.target, r.from = 0, 0, 0
	if r.provide != 0 {
		r.provide.Release()
		r.provide = 0
	}
}
