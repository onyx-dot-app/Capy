package audio

import (
	"context"
	"fmt"
	"runtime"
	"unsafe"

	"github.com/ebitengine/purego/objc"
)

// AVFoundation constants.
const (
	avAuthorizationNotDetermined = 0
	avAuthorizationAuthorized    = 3
	avAudioPCMFormatInt16        = 3
	avConverterInputHaveData     = 0
	avConverterInputNoDataNow    = 1
	avConverterOutputError       = 3
)

// Mic captures the default input device with AVAudioEngine.
type Mic struct {
	sink *sink
}

// NewMic returns a microphone Source.
func NewMic() *Mic { return &Mic{} }

// Err implements Source.
func (m *Mic) Err() error {
	if m.sink == nil {
		return nil
	}
	return m.sink.errOrNil()
}

// Start implements Source. It asks for microphone access the first time.
func (m *Mic) Start(ctx context.Context) (<-chan []byte, error) {
	if err := load(); err != nil {
		return nil, err
	}
	if err := requestMicAccess(ctx); err != nil {
		return nil, err
	}
	s := newSink()
	var c *micCapture
	var err error
	withPool(func() { c, err = startMic(s) })
	if err != nil {
		return nil, err
	}
	m.sink = s
	go func() {
		<-ctx.Done()
		withPool(c.stop)
		s.close()
	}()
	return s.ch, nil
}

// requestMicAccess returns nil once the app may use the microphone.
func requestMicAccess(ctx context.Context) error {
	device, err := class("AVCaptureDevice")
	if err != nil {
		return err
	}
	status := objc.Send[int](device, sel("authorizationStatusForMediaType:"), avMediaTypeAudio)
	switch status {
	case avAuthorizationAuthorized:
		return nil
	case avAuthorizationNotDetermined:
	default:
		return &PermissionError{What: "microphone", Pane: MicrophonePane}
	}
	granted := make(chan bool, 1)
	block := objc.NewBlock(func(_ objc.Block, ok bool) { granted <- ok })
	defer block.Release()
	send(device, "requestAccessForMediaType:completionHandler:", avMediaTypeAudio, block)
	select {
	case ok := <-granted:
		if !ok {
			return &PermissionError{What: "microphone", Pane: MicrophonePane}
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// micCapture is a running AVAudioEngine with a tap on its input node that
// converts each buffer to 16 kHz mono PCM16.
type micCapture struct {
	engine, input objc.ID
	tap           objc.Block
	r             *resampler
}

func startMic(s *sink) (*micCapture, error) {
	engineClass, err := class("AVAudioEngine")
	if err != nil {
		return nil, err
	}
	c := &micCapture{}
	c.engine = send(send(engineClass, "alloc"), "init")
	c.input = send(c.engine, "inputNode")
	hw := send(c.input, "outputFormatForBus:", uint(0))
	if objc.Send[float64](hw, sel("sampleRate")) == 0 {
		send(c.engine, "release")
		return nil, ErrNoMicrophone
	}
	if c.r, err = newResampler(hw, s, "microphone audio"); err != nil {
		send(c.engine, "release")
		return nil, err
	}
	c.tap = objc.NewBlock(func(_ objc.Block, buf, _ objc.ID) { c.r.convert(buf) })
	send(c.input, "installTapOnBus:bufferSize:format:block:", uint(0), uint32(4096), hw, c.tap)

	send(c.engine, "prepare")
	errp := new(objc.ID)
	ok := sendBool(c.engine, "startAndReturnError:", unsafe.Pointer(errp))
	runtime.KeepAlive(errp)
	if !ok {
		err := nsError(*errp)
		send(c.input, "removeTapOnBus:", uint(0))
		c.release()
		return nil, fmt.Errorf("audio: start microphone: %w", err)
	}
	return c, nil
}

// stop ends capture. The tap and engine stop first, as a callback in
// progress holds the resampler until it returns.
func (c *micCapture) stop() {
	send(c.input, "removeTapOnBus:", uint(0))
	send(c.engine, "stop")
	c.release()
}

func (c *micCapture) release() {
	c.r.close()
	send(c.engine, "release")
	c.engine = 0
	c.tap.Release()
	c.tap = 0
}
