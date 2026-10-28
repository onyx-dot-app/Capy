package audio

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"github.com/ebitengine/purego/objc"
)

// System captures what the Mac is playing through a Core Audio process tap
// (macOS 14.2+), excluding Capy's own sounds. A tap needs the System Audio
// Recording permission only, where ScreenCaptureKit needs Screen Recording.
type System struct {
	sink *sink
}

// NewSystem returns a system audio Source.
func NewSystem() *System { return &System{} }

// Err implements Source.
func (s *System) Err() error {
	if s.sink == nil {
		return nil
	}
	return s.sink.errOrNil()
}

// Start implements Source. It asks for System Audio Recording access the
// first time.
func (s *System) Start(ctx context.Context) (<-chan []byte, error) {
	if err := load(); err != nil {
		return nil, err
	}
	if err := requestSystemAudioAccess(ctx); err != nil {
		return nil, err
	}
	out := newSink()
	var c *systemCapture
	var err error
	withPool(func() { c, err = startSystem(out) })
	if err != nil {
		return nil, err
	}
	s.sink = out
	go func() {
		<-ctx.Done()
		withPool(c.stop)
		out.close()
	}()
	return out.ch, nil
}

// TCCAccessPreflight results.
const (
	tccGranted = 0
	tccDenied  = 1
)

// requestSystemAudioAccess returns nil once the app may record system
// audio. A tap without the permission records silence rather than fail,
// so the permission is checked first, through private TCC calls. Without
// them, it returns nil and the tap asks macOS itself.
func requestSystemAudioAccess(ctx context.Context) error {
	if tccAccessPreflight == nil {
		return nil
	}
	denied := &PermissionError{What: "system audio recording", Pane: SystemAudioPane}
	granted := make(chan bool, 1)
	block := objc.NewBlock(func(_ objc.Block, ok bool) { granted <- ok })
	defer block.Release()
	var status int32
	withPool(func() {
		service := nsString("kTCCServiceAudioCapture")
		if status = tccAccessPreflight(service, 0); status != tccGranted && status != tccDenied {
			tccAccessRequest(service, 0, block)
		}
	})
	switch status {
	case tccGranted:
		return nil
	case tccDenied:
		return denied
	}
	select {
	case ok := <-granted:
		if !ok {
			return denied
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// systemCapture is a running global tap, read through a private aggregate
// device whose only input is the tap.
type systemCapture struct {
	tap, device uint32 // AudioObjectIDs; 0 when not created
	proc        uintptr
	io          objc.Block
	r           *resampler
}

func startSystem(s *sink) (*systemCapture, error) {
	if audioHardwareCreateProcessTap == nil {
		return nil, errors.New("audio: capturing the Mac's audio needs macOS 14.2 or later")
	}
	c := &systemCapture{}
	if err := c.open(s); err != nil {
		c.stop()
		return nil, err
	}
	return c, nil
}

func (c *systemCapture) open(s *sink) error {
	tapClass, err := class("CATapDescription")
	if err != nil {
		return err
	}
	exclude := send(objc.ID(objc.GetClass("NSArray")), "array")
	if self := processObject(); self != 0 {
		exclude = send(objc.ID(objc.GetClass("NSArray")), "arrayWithObject:",
			send(objc.ID(objc.GetClass("NSNumber")), "numberWithUnsignedInt:", self))
	}
	desc := send(send(tapClass, "alloc"), "initMonoGlobalTapButExcludeProcesses:", exclude)
	defer send(desc, "release")
	send(desc, "setPrivate:", true)
	if st := audioHardwareCreateProcessTap(desc, &c.tap); st != 0 {
		c.tap = 0
		return osStatus("create the system audio tap", st)
	}

	var asbd audioStreamBasicDescription
	addr := audioObjectPropertyAddress{kAudioTapPropertyFormat, kAudioObjectPropertyScopeGlobal, kAudioObjectPropertyElementMain}
	size := uint32(unsafe.Sizeof(asbd))
	if st := audioObjectGetPropertyData(c.tap, &addr, 0, nil, &size, unsafe.Pointer(&asbd)); st != 0 {
		return osStatus("read the system audio format", st)
	}
	formatClass, _ := class("AVAudioFormat")
	format := send(send(formatClass, "alloc"), "initWithStreamDescription:", unsafe.Pointer(&asbd))
	if format == 0 {
		return fmt.Errorf("audio: system audio arrived in an unexpected format (%.0f Hz, %d channels)", asbd.SampleRate, asbd.ChannelsPerFrame)
	}
	c.r, err = newResampler(format, s, "system audio")
	send(format, "release")
	if err != nil {
		return err
	}

	tapUID := send(send(desc, "UUID"), "UUIDString")
	if st := audioHardwareCreateAggregateDevice(aggregateDescription(tapUID), &c.device); st != 0 {
		c.device = 0
		return osStatus("create the system audio device", st)
	}
	c.io = objc.NewBlock(func(_ objc.Block, _, in, _, _, _ unsafe.Pointer) {
		if in != nil {
			c.r.convertList(in)
		}
	})
	if st := audioDeviceCreateIOProcIDWithBlock(&c.proc, c.device, 0, c.io); st != 0 {
		c.proc = 0
		return osStatus("read the system audio device", st)
	}
	if st := audioDeviceStart(c.device, c.proc); st != 0 {
		return osStatus("start the system audio device", st)
	}
	return nil
}

// processObject is Capy's own CoreAudio process object, 0 before Capy has
// used audio.
func processObject() uint32 {
	pid := int32(os.Getpid())
	var obj uint32
	addr := audioObjectPropertyAddress{kAudioHardwarePropertyTranslatePIDToProcessObject, kAudioObjectPropertyScopeGlobal, kAudioObjectPropertyElementMain}
	size := uint32(unsafe.Sizeof(obj))
	if audioObjectGetPropertyData(kAudioObjectSystemObject, &addr, uint32(unsafe.Sizeof(pid)), unsafe.Pointer(&pid), &size, unsafe.Pointer(&obj)) != 0 {
		return 0
	}
	return obj
}

// aggregateDescription describes a private aggregate device whose only
// input is the tap with tapUID, an NSString.
func aggregateDescription(tapUID objc.ID) objc.ID {
	dict := func(kv ...any) objc.ID {
		d := send(objc.ID(objc.GetClass("NSMutableDictionary")), "dictionary")
		for i := 0; i < len(kv); i += 2 {
			send(d, "setObject:forKey:", kv[i+1], nsString(kv[i].(string)))
		}
		return d
	}
	number := objc.ID(objc.GetClass("NSNumber"))
	tap := dict("uid", tapUID, "drift", send(number, "numberWithBool:", true))
	return dict(
		"name", nsString("Capy System Audio"),
		"uid", send(send(objc.ID(objc.GetClass("NSUUID")), "UUID"), "UUIDString"),
		"private", send(number, "numberWithBool:", true),
		"taps", send(objc.ID(objc.GetClass("NSArray")), "arrayWithObject:", tap),
	)
}

// stop ends capture and frees what open created, in reverse.
func (c *systemCapture) stop() {
	if c.proc != 0 {
		audioDeviceStop(c.device, c.proc)
		audioDeviceDestroyIOProcID(c.device, c.proc)
		c.proc = 0
	}
	if c.device != 0 {
		audioHardwareDestroyAggregateDevice(c.device)
		c.device = 0
	}
	if c.io != 0 {
		c.io.Release()
		c.io = 0
	}
	if c.r != nil {
		c.r.close()
		c.r = nil
	}
	if c.tap != 0 {
		audioHardwareDestroyProcessTap(c.tap)
		c.tap = 0
	}
}

// osStatus describes a failed CoreAudio call.
func osStatus(what string, st int32) error {
	return fmt.Errorf("audio: %s: OSStatus %d", what, st)
}

// nsString returns an autoreleased NSString.
func nsString(s string) objc.ID {
	b := append([]byte(s), 0)
	str := send(objc.ID(objc.GetClass("NSString")), "stringWithUTF8String:", unsafe.Pointer(&b[0]))
	runtime.KeepAlive(b)
	return str
}
