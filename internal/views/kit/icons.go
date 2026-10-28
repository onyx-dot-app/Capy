package kit

import (
	"github.com/egoist/mygo/ui"
)

// Window control and tab icons, drawn in the color of the text.
var (
	MinimizeIcon     = StrokeIcon(`<path d="M5 12h14"/>`)
	MaximizeIcon     = StrokeIcon(`<path d="M14 4h6v6M10 20H4v-6M20 4l-6 6M4 20l6-6"/>`)
	RestoreIcon      = StrokeIcon(`<path d="M4 14h6v6M20 10h-6V4M14 10l6-6M10 14l-6 6"/>`)
	ExpandIcon       = MaximizeIcon
	CloseIcon        = StrokeIcon(`<path d="M6 6l12 12M18 6L6 18"/>`)
	SummaryIcon      = StrokeIcon(`<path d="M12 3l1.8 4.9L19 9.7l-5.2 1.8L12 16.4l-1.8-4.9L5 9.7l5.2-1.8z"/><path d="M19 15l.7 1.8 1.8.7-1.8.7L19 20l-.7-1.8-1.8-.7 1.8-.7z"/>`)
	TranscriptIcon   = StrokeIcon(`<path d="M4 6h16M4 11h16M4 16h10"/>`)
	ChevronDownIcon  = StrokeIcon(`<path d="M7 10l5 5 5-5"/>`)
	ChevronRightIcon = StrokeIcon(`<path d="M10 7l5 5-5 5"/>`)
	CopyIcon         = StrokeIcon(`<rect x="8" y="8" width="12" height="12" rx="2"/><path d="M16 8V6a2 2 0 0 0-2-2H6a2 2 0 0 0-2 2v8a2 2 0 0 0 2 2h2"/>`)
	ShareIcon        = StrokeIcon(`<path d="M12 3v12M8 7l4-4 4 4M7 11H6a2 2 0 0 0-2 2v6a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-6a2 2 0 0 0-2-2h-1"/>`)
	RetryIcon        = StrokeIcon(`<path d="M20 12a8 8 0 1 1-2.34-5.66M20 4v5h-5"/>`)
	CaptionsIcon     = StrokeIcon(`<path d="M4 5h16a1 1 0 0 1 1 1v10a1 1 0 0 1-1 1h-9l-4 3v-3H4a1 1 0 0 1-1-1V6a1 1 0 0 1 1-1z"/><path d="M7 10h4M13 10h4M7 13h7"/>`)
	WarnIcon         = StrokeIcon(`<path d="M12 4l9 16H3z"/><path d="M12 10v4M12 17v.01"/>`)
	KeyIcon          = StrokeIcon(`<circle cx="7.5" cy="12" r="3.5"/><path d="M11 12h10M18 12v3M21 12v2"/>`)
	MicOffIcon       = StrokeIcon(`<path d="M15 9.5V6a3 3 0 0 0-6 0v5a3 3 0 0 0 4.5 2.6M5.5 11a6.5 6.5 0 0 0 10.6 5M18.5 11c0 .7-.1 1.3-.3 1.9M12 17.5V21M4 4l16 16"/>`)
	UpDownIcon       = StrokeIcon(`<path d="M8 9l4-4 4 4M8 15l4 4 4-4"/>`)
	CheckIcon        = StrokeIcon(`<path d="M5 12.5l4.5 4.5L19 7.5"/>`)
	MicIcon          = StrokeIcon(`<rect x="9" y="3" width="6" height="11" rx="3"/><path d="M5.5 11a6.5 6.5 0 0 0 13 0M12 17.5V21"/>`)
	ImportIcon       = StrokeIcon(`<path d="M12 3.5v10.5M8 10l4 4 4-4"/><path d="M4 14v3.5A2.5 2.5 0 0 0 6.5 20h11a2.5 2.5 0 0 0 2.5-2.5V14"/>`)
	AudioFileIcon    = StrokeIcon(`<path d="M14 3H7.5A1.5 1.5 0 0 0 6 4.5v15A1.5 1.5 0 0 0 7.5 21h9a1.5 1.5 0 0 0 1.5-1.5V7z"/><path d="M14 3v4h4"/><path d="M9.5 13v3M12 11.5v6M14.5 13.5v2"/>`)
	BellIcon         = StrokeIcon(`<path d="M6 16.5V11a6 6 0 0 1 12 0v5.5l1.5 2h-15z"/><path d="M10 20.5a2 2 0 0 0 4 0"/>`)
	CircleIcon       = StrokeIcon(`<circle cx="12" cy="12" r="7.5"/>`)
	WaveformIcon     = StrokeIcon(`<path d="M3 12h1M7 8v8M11 4v16M15 7v10M19 10v4M21 12h0"/>`)
	LaptopIcon       = StrokeIcon(`<rect x="5" y="5" width="14" height="10" rx="1.5"/><path d="M3 19h18"/>`)
	InlineIcon       = StrokeIcon(`<rect x="3" y="4" width="18" height="16" rx="2.5"/><path d="M7 10h10M7 14h7"/>`)
	SideBySideIcon   = StrokeIcon(`<rect x="3" y="4" width="18" height="16" rx="2.5"/><path d="M12 4v16"/>`)
	TerminalIcon     = StrokeIcon(`<rect x="3" y="4" width="18" height="16" rx="2.5"/><path d="M7 9.5l3 2.5-3 2.5M12.5 15H17"/>`)
	AlertIcon        = StrokeIcon(`<circle cx="12" cy="12" r="9"/><path d="M12 7.5v5.5M12 16.5v.01"/>`)
	TranslateIcon    = StrokeIcon(`<path d="M5 8l6 6M4 14l6-6 2-3M2 5h12M7 2h1M22 22l-5-10-5 10M14 18h6"/>`)
	InfoIcon         = StrokeIcon(`<circle cx="12" cy="12" r="9"/><path d="M12 11v5.5M12 7.5v.01"/>`)
	TrashIcon        = StrokeIcon(`<path d="M4 6.5h16M9.5 6.5v-2h5v2M6.5 6.5l1 13h9l1-13M10 10.5v6M14 10.5v6"/>`)
	SettingsIcon     = StrokeIcon(`<path d="M18.8 10.2L21.4 10.6L21.4 13.4L18.8 13.8A7 7 0 0 1 18.1 15.5L19.6 17.6L17.6 19.6L15.5 18.1A7 7 0 0 1 13.8 18.8L13.4 21.4L10.6 21.4L10.2 18.8A7 7 0 0 1 8.5 18.1L6.4 19.6L4.4 17.6L5.9 15.5A7 7 0 0 1 5.2 13.8L2.6 13.4L2.6 10.6L5.2 10.2A7 7 0 0 1 5.9 8.5L4.4 6.4L6.4 4.4L8.5 5.9A7 7 0 0 1 10.2 5.2L10.6 2.6L13.4 2.6L13.8 5.2A7 7 0 0 1 15.5 5.9L17.6 4.4L19.6 6.4L18.1 8.5A7 7 0 0 1 18.8 10.2z"/><circle cx="12" cy="12" r="3"/>`)
)

// StrokeIcon is a 24×24 icon of round 2-pixel strokes.
func StrokeIcon(paths string) *ui.SVG {
	return ui.MustParseSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="#000" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">` + paths + `</svg>`))
}
