package audio

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

// Frameworks and C functions used by the capture sources, loaded once.
// mygo builds with CGO_ENABLED=0, so native APIs are reached through the
// Objective-C runtime with purego instead of cgo.
var (
	loadOnce sync.Once
	errLoad  error

	poolPush func() uintptr
	poolPop  func(uintptr)

	audioObjectGetPropertyData          func(obj uint32, addr *audioObjectPropertyAddress, qualSize uint32, qual unsafe.Pointer, size *uint32, out unsafe.Pointer) int32
	audioHardwareCreateAggregateDevice  func(desc objc.ID, out *uint32) int32
	audioHardwareDestroyAggregateDevice func(device uint32) int32
	audioDeviceCreateIOProcIDWithBlock  func(out *uintptr, device uint32, queue uintptr, block objc.Block) int32
	audioDeviceStart                    func(device uint32, proc uintptr) int32
	audioDeviceStop                     func(device uint32, proc uintptr) int32
	audioDeviceDestroyIOProcID          func(device uint32, proc uintptr) int32

	// Process taps arrived in macOS 14.2; nil before.
	audioHardwareCreateProcessTap  func(desc objc.ID, out *uint32) int32
	audioHardwareDestroyProcessTap func(tap uint32) int32

	// The private TCC calls that check and request a permission; nil
	// when this macOS lacks them.
	tccAccessPreflight func(service, options objc.ID) int32
	tccAccessRequest   func(service, options objc.ID, done objc.Block)

	avMediaTypeAudio objc.ID
)

// audioObjectPropertyAddress mirrors CoreAudio's AudioObjectPropertyAddress.
type audioObjectPropertyAddress struct {
	Selector, Scope, Element uint32
}

// CoreAudio objects and properties.
const (
	kAudioObjectSystemObject                          = 1
	kAudioObjectPropertyScopeGlobal                   = 0x676c6f62 // 'glob'
	kAudioObjectPropertyElementMain                   = 0
	kAudioHardwarePropertyTranslatePIDToProcessObject = 0x69643270 // 'id2p'
	kAudioTapPropertyFormat                           = 0x74666d74 // 'tfmt'
)

// audioStreamBasicDescription mirrors CoreAudio's AudioStreamBasicDescription.
type audioStreamBasicDescription struct {
	SampleRate       float64
	FormatID         uint32
	FormatFlags      uint32
	BytesPerPacket   uint32
	FramesPerPacket  uint32
	BytesPerFrame    uint32
	ChannelsPerFrame uint32
	BitsPerChannel   uint32
	Reserved         uint32
}

func load() error {
	loadOnce.Do(func() {
		open := func(path string) uintptr {
			if errLoad != nil {
				return 0
			}
			h, err := purego.Dlopen(path, purego.RTLD_GLOBAL|purego.RTLD_NOW)
			if err != nil {
				errLoad = fmt.Errorf("audio: load %s: %w", path, err)
			}
			return h
		}
		libObjC := open("/usr/lib/libobjc.A.dylib")
		avf := open("/System/Library/Frameworks/AVFoundation.framework/AVFoundation")
		ca := open("/System/Library/Frameworks/CoreAudio.framework/CoreAudio")
		if errLoad != nil {
			return
		}
		purego.RegisterLibFunc(&poolPush, libObjC, "objc_autoreleasePoolPush")
		purego.RegisterLibFunc(&poolPop, libObjC, "objc_autoreleasePoolPop")
		purego.RegisterLibFunc(&audioObjectGetPropertyData, ca, "AudioObjectGetPropertyData")
		purego.RegisterLibFunc(&audioHardwareCreateAggregateDevice, ca, "AudioHardwareCreateAggregateDevice")
		purego.RegisterLibFunc(&audioHardwareDestroyAggregateDevice, ca, "AudioHardwareDestroyAggregateDevice")
		purego.RegisterLibFunc(&audioDeviceCreateIOProcIDWithBlock, ca, "AudioDeviceCreateIOProcIDWithBlock")
		purego.RegisterLibFunc(&audioDeviceStart, ca, "AudioDeviceStart")
		purego.RegisterLibFunc(&audioDeviceStop, ca, "AudioDeviceStop")
		purego.RegisterLibFunc(&audioDeviceDestroyIOProcID, ca, "AudioDeviceDestroyIOProcID")
		// RegisterLibFunc panics on a missing symbol, as the taps are before 14.2.
		if create, err := purego.Dlsym(ca, "AudioHardwareCreateProcessTap"); err == nil {
			if destroy, err := purego.Dlsym(ca, "AudioHardwareDestroyProcessTap"); err == nil {
				purego.RegisterFunc(&audioHardwareCreateProcessTap, create)
				purego.RegisterFunc(&audioHardwareDestroyProcessTap, destroy)
			}
		}
		if tcc, err := purego.Dlopen("/System/Library/PrivateFrameworks/TCC.framework/TCC", purego.RTLD_GLOBAL|purego.RTLD_NOW); err == nil {
			if preflight, err := purego.Dlsym(tcc, "TCCAccessPreflight"); err == nil {
				if request, err := purego.Dlsym(tcc, "TCCAccessRequest"); err == nil {
					purego.RegisterFunc(&tccAccessPreflight, preflight)
					purego.RegisterFunc(&tccAccessRequest, request)
				}
			}
		}

		sym, err := purego.Dlsym(avf, "AVMediaTypeAudio")
		if err != nil {
			errLoad = fmt.Errorf("audio: AVMediaTypeAudio: %w", err)
			return
		}
		avMediaTypeAudio = **(**objc.ID)(unsafe.Pointer(&sym))
	})
	return errLoad
}

var (
	selMu    sync.Mutex
	selCache = map[string]objc.SEL{}
)

// sel returns the cached selector for name.
func sel(name string) objc.SEL {
	selMu.Lock()
	defer selMu.Unlock()
	s, ok := selCache[name]
	if !ok {
		s = objc.RegisterName(name)
		selCache[name] = s
	}
	return s
}

// class returns the class named name, or an error when this macOS lacks it.
func class(name string) (objc.ID, error) {
	c := objc.GetClass(name)
	if c == 0 {
		return 0, fmt.Errorf("audio: Objective-C class %s is not available", name)
	}
	return objc.ID(c), nil
}

func send(obj objc.ID, selector string, args ...any) objc.ID {
	return obj.Send(sel(selector), args...)
}

func sendBool(obj objc.ID, selector string, args ...any) bool {
	return objc.Send[bool](obj, sel(selector), args...)
}

// withPool runs fn in an autorelease pool on a locked OS thread.
func withPool(fn func()) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	pool := poolPush()
	defer poolPop(pool)
	fn()
}

// goString copies an NSString.
func goString(str objc.ID) string {
	if str == 0 {
		return ""
	}
	p := objc.Send[*byte](str, sel("UTF8String"))
	if p == nil {
		return ""
	}
	n := objc.Send[int](str, sel("lengthOfBytesUsingEncoding:"), 4) // NSUTF8StringEncoding
	return string(unsafe.Slice(p, n))
}

// nsError converts an NSError into a Go error; nil for a nil NSError.
func nsError(err objc.ID) error {
	if err == 0 {
		return nil
	}
	return errors.New(goString(send(err, "localizedDescription")))
}
