// parse_modes.go holds the pure string parsers behind the Linux display
// backends (display_linux.go). They are kept platform-independent and
// untagged — like PickBestMode in display.go — so display_test.go can
// exercise the brittle xrandr/cvt text parsing on any developer machine
// without an X server. The exec plumbing that feeds them lives in the
// build-tagged display_linux.go.
package display

import (
	"regexp"
	"strconv"
	"strings"
)

// parseModeWxH parses an "WxH" token (e.g. "1920x1080", tolerating a
// trailing interlace/junk suffix like "1920x1080i") into width/height.
func parseModeWxH(s string) (w, h int, ok bool) {
	i := strings.IndexByte(s, 'x')
	if i <= 0 || i+1 >= len(s) {
		return 0, 0, false
	}
	wStr := s[:i]
	rest := s[i+1:]
	// height may carry a trailing non-digit (e.g. the 'i' in "...x1080i").
	j := 0
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		j++
	}
	hStr := rest[:j]
	wv, e1 := strconv.Atoi(wStr)
	hv, e2 := strconv.Atoi(hStr)
	if e1 != nil || e2 != nil || wv <= 0 || hv <= 0 {
		return 0, 0, false
	}
	return wv, hv, true
}

// parseHz rounds an xrandr refresh token ("60.00", "59.94") to an int.
// Returns 0 when the token is not a number.
func parseHz(s string) int {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 {
		return 0
	}
	return int(f + 0.5)
}

// parseXrandrQuery parses `xrandr --query` output. It returns the first
// CONNECTED output's name, that output's advertised modes, and the
// currently-active mode (the one whose refresh carries a trailing '*').
//
// Layout it parses (column 1 of an indented line is the WxH; a rate with
// a trailing '*' marks the active mode, '+' the preferred one):
//
//	Screen 0: minimum 320 x 200, current 1920 x 1080, maximum 16384 x 16384
//	Virtual-1 connected primary 1920x1080+0+0 (normal ...) 0mm x 0mm
//	   1920x1080     60.00*+
//	   1280x1024     60.02
//
// A non-RandR-1.2 server presents the output as the literal "default".
func parseXrandrQuery(out string) (output string, modes []Mode, current Mode) {
	inChosen := false
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		indented := line[0] == ' ' || line[0] == '\t'
		if !indented {
			// Candidate output-header line: "<name> connected|disconnected ..."
			// or the legacy "default connected ...".
			fields := strings.Fields(line)
			if len(fields) < 2 {
				inChosen = false
				continue
			}
			name, status := fields[0], fields[1]
			if status == "connected" || status == "disconnected" || name == "default" {
				// Lock onto the FIRST connected output (a VM has one: Virtual-1).
				if output == "" && (status == "connected" || name == "default") {
					output = name
					inChosen = true
				} else {
					inChosen = (name == output)
				}
			} else {
				inChosen = false
			}
			continue
		}
		if !inChosen {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		w, h, ok := parseModeWxH(fields[0])
		if !ok {
			continue
		}
		m := Mode{Width: w, Height: h}
		for _, tok := range fields[1:] {
			isCurrent := strings.Contains(tok, "*")
			hz := parseHz(strings.TrimRight(tok, "*+ "))
			if hz > 0 && m.Hz == 0 {
				m.Hz = hz
			}
			if isCurrent {
				current = Mode{Width: w, Height: h, Hz: hz}
			}
		}
		modes = append(modes, m)
	}
	return output, modes, current
}

// parseCvtModeline extracts the modeline name and its timing arguments from
// `cvt W H R` output, ready to feed to `xrandr --newmode <name> <args...>`.
//
//	# 1920x1080 59.96 Hz (CVT 2.07M9) ...
//	Modeline "1920x1080_60.00"  173.00  1920 2048 2248 2576  1080 1083 1088 1125 -hsync +vsync
func parseCvtModeline(out string) (name string, args []string, ok bool) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Modeline") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, "Modeline"))
		if rest == "" {
			continue
		}
		if rest[0] == '"' {
			end := strings.IndexByte(rest[1:], '"')
			if end < 0 {
				continue
			}
			name = rest[1 : 1+end]
			rest = strings.TrimSpace(rest[1+end+1:])
		} else {
			f := strings.Fields(rest)
			name = f[0]
			rest = strings.TrimSpace(strings.TrimPrefix(rest, f[0]))
		}
		args = strings.Fields(rest)
		// A valid modeline is: pclk + 4 htimings + 4 vtimings (+ flags).
		if name != "" && len(args) >= 9 {
			return name, args, true
		}
	}
	return "", nil, false
}

// mutterMode is one display mode parsed from GNOME Mutter's
// org.gnome.Mutter.DisplayConfig.GetCurrentState (via gdbus). The id string
// is what ApplyMonitorsConfig requires to select the mode.
type mutterMode struct {
	id      string
	w, h    int
	current bool
}

var (
	// Top-level serial: gdbus prints GetCurrentState as "(uint32 N, [ ... ".
	reMutterSerial = regexp.MustCompile(`uint32\s+(\d+)`)
	// Monitor spec opens the monitors array: "[(('Virtual-1', 'vendor', ...".
	// The connector is the first quoted string after the "((".
	reMutterConn = regexp.MustCompile(`\(\('([^']+)'`)
	// Each mode tuple is ('id', W, H, refresh(d), preferred_scale(d),
	// [supported_scales], {props}). A quoted id FOLLOWED BY two ints is what
	// distinguishes a mode from the connector tuple (quoted-then-quoted).
	reMutterMode = regexp.MustCompile(`'([^']*)',\s*(\d+),\s*(\d+),\s*[0-9.eE+-]+,\s*[0-9.eE+-]+,\s*\[[^\]]*\],\s*(\{[^}]*\})`)
)

// parseMutterCurrentState extracts the config serial (required fresh by
// ApplyMonitorsConfig), the primary connector name, and the advertised modes
// from a gdbus GetCurrentState response. A mode is marked current when its
// properties dict carries 'is-current': <true>.
func parseMutterCurrentState(out string) (serial, connector string, modes []mutterMode) {
	if m := reMutterSerial.FindStringSubmatch(out); m != nil {
		serial = m[1]
	}
	if m := reMutterConn.FindStringSubmatch(out); m != nil {
		connector = m[1]
	}
	for _, m := range reMutterMode.FindAllStringSubmatch(out, -1) {
		w, _ := strconv.Atoi(m[2])
		h, _ := strconv.Atoi(m[3])
		if w <= 0 || h <= 0 {
			continue
		}
		modes = append(modes, mutterMode{
			id:      m[1],
			w:       w,
			h:       h,
			current: strings.Contains(m[4], "'is-current': <true>"),
		})
	}
	return serial, connector, modes
}
