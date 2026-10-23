package transcode

import (
	"fmt"
	"strconv"
)

// Settings are the video encoder choices an admin can change at runtime. The zero value means the defaults.
type Settings struct {
	// HWAccel is "none", "nvenc", "qsv", "vaapi" or "amf".
	HWAccel string
	// Preset is a libx264 preset name; hardware encoders map it to their closest equivalent.
	Preset string
	// CRF is the quality target, 16 (best) to 32 (smallest).
	CRF int
	// Threads limits encoder threads; 0 lets ffmpeg decide.
	Threads int
	// MaxBitrateKbps caps the video bitrate; 0 means no cap.
	MaxBitrateKbps int
}

const (
	defaultPreset = "veryfast"
	defaultCRF    = 23
	vaapiDevice   = "/dev/dri/renderD128"
)

var presets = []string{"ultrafast", "superfast", "veryfast", "faster", "fast", "medium", "slow"}

// ValidHWAccel reports whether name is a supported hardware acceleration mode.
func ValidHWAccel(name string) bool {
	switch name {
	case "none", "nvenc", "qsv", "vaapi", "amf":
		return true
	}
	return false
}

// ValidPreset reports whether name is a supported encoder preset.
func ValidPreset(name string) bool {
	for _, preset := range presets {
		if preset == name {
			return true
		}
	}
	return false
}

func (s Settings) resolved() Settings {
	if !ValidHWAccel(s.HWAccel) {
		s.HWAccel = "none"
	}
	if !ValidPreset(s.Preset) {
		s.Preset = defaultPreset
	}
	if s.CRF < 16 || s.CRF > 32 {
		s.CRF = defaultCRF
	}
	if s.Threads < 0 {
		s.Threads = 0
	}
	if s.MaxBitrateKbps < 0 {
		s.MaxBitrateKbps = 0
	}
	return s
}

// inputArgs are the ffmpeg options that must come before -i.
func (s Settings) inputArgs() []string {
	if s.resolved().HWAccel == "vaapi" {
		return []string{"-vaapi_device", vaapiDevice}
	}
	return nil
}

// videoFilter returns the -vf value, or "" when no filter is needed.
func (s Settings) videoFilter(scaleHeight int) string {
	filter := ""
	if scaleHeight > 0 {
		filter = fmt.Sprintf("scale=-2:%d", scaleHeight)
	}
	if s.resolved().HWAccel == "vaapi" {
		if filter != "" {
			filter += ","
		}
		filter += "format=nv12,hwupload"
	}
	return filter
}

func nvencPreset(preset string) string {
	switch preset {
	case "ultrafast", "superfast":
		return "p1"
	case "veryfast":
		return "p2"
	case "faster", "fast":
		return "p3"
	case "medium":
		return "p4"
	}
	return "p5"
}

func qsvPreset(preset string) string {
	if preset == "ultrafast" || preset == "superfast" {
		return "veryfast"
	}
	return preset
}

func amfQuality(preset string) string {
	switch preset {
	case "medium":
		return "balanced"
	case "slow":
		return "quality"
	}
	return "speed"
}

// videoArgs returns the video encoder options (everything from -c:v on, except the filter).
func (s Settings) videoArgs() []string {
	s = s.resolved()
	crf := strconv.Itoa(s.CRF)
	var args []string
	switch s.HWAccel {
	case "nvenc":
		args = []string{"-c:v", "h264_nvenc", "-preset", nvencPreset(s.Preset), "-rc", "vbr", "-cq", crf, "-b:v", "0", "-profile:v", "high", "-pix_fmt", "yuv420p"}
	case "qsv":
		args = []string{"-c:v", "h264_qsv", "-preset", qsvPreset(s.Preset), "-global_quality", crf, "-profile:v", "high", "-pix_fmt", "nv12"}
	case "vaapi":
		args = []string{"-c:v", "h264_vaapi", "-qp", crf, "-profile:v", "high"}
	case "amf":
		args = []string{"-c:v", "h264_amf", "-quality", amfQuality(s.Preset), "-rc", "cqp", "-qp_i", crf, "-qp_p", crf, "-profile:v", "high", "-pix_fmt", "yuv420p"}
	default:
		args = []string{"-c:v", "libx264", "-preset", s.Preset, "-crf", crf, "-profile:v", "high", "-pix_fmt", "yuv420p"}
	}
	if s.MaxBitrateKbps > 0 {
		args = append(args, "-maxrate", fmt.Sprintf("%dk", s.MaxBitrateKbps), "-bufsize", fmt.Sprintf("%dk", s.MaxBitrateKbps*2))
	}
	if s.Threads > 0 {
		args = append(args, "-threads", strconv.Itoa(s.Threads))
	}
	return args
}
