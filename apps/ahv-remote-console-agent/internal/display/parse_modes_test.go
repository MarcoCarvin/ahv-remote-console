package display

import (
	"reflect"
	"testing"
)

func TestParseModeWxH(t *testing.T) {
	cases := []struct {
		in   string
		w, h int
		ok   bool
	}{
		{"1920x1080", 1920, 1080, true},
		{"1280x1024i", 1280, 1024, true}, // trailing interlace flag
		{"800x600", 800, 600, true},
		{"", 0, 0, false},
		{"1920", 0, 0, false},
		{"x1080", 0, 0, false},
		{"1920x", 0, 0, false},
		{"axb", 0, 0, false},
	}
	for _, c := range cases {
		w, h, ok := parseModeWxH(c.in)
		if w != c.w || h != c.h || ok != c.ok {
			t.Errorf("parseModeWxH(%q) = (%d,%d,%v); want (%d,%d,%v)", c.in, w, h, ok, c.w, c.h, c.ok)
		}
	}
}

func TestParseXrandrQuery(t *testing.T) {
	const out = `Screen 0: minimum 320 x 200, current 1920 x 1080, maximum 16384 x 16384
Virtual-1 connected primary 1920x1080+0+0 (normal left inverted right x axis y axis) 0mm x 0mm
   1920x1080     60.00*+  59.94
   1680x1050     59.95
   1280x1024     60.02
   800x600       60.32    56.25`
	output, modes, current := parseXrandrQuery(out)
	if output != "Virtual-1" {
		t.Fatalf("output = %q; want Virtual-1", output)
	}
	want := []Mode{
		{Width: 1920, Height: 1080, Hz: 60},
		{Width: 1680, Height: 1050, Hz: 60},
		{Width: 1280, Height: 1024, Hz: 60},
		{Width: 800, Height: 600, Hz: 60},
	}
	if !reflect.DeepEqual(modes, want) {
		t.Errorf("modes = %+v; want %+v", modes, want)
	}
	if current.Width != 1920 || current.Height != 1080 {
		t.Errorf("current = %+v; want 1920x1080 (the '*' mode)", current)
	}
}

func TestParseXrandrQuery_LegacyDefaultOutput(t *testing.T) {
	const out = `Screen 0: minimum 640 x 480, current 1024 x 768, maximum 1024 x 768
default connected 1024x768+0+0 0mm x 0mm
   1024x768      0.00*`
	output, modes, current := parseXrandrQuery(out)
	if output != "default" {
		t.Fatalf("output = %q; want default", output)
	}
	if len(modes) != 1 || modes[0].Width != 1024 || modes[0].Height != 768 {
		t.Errorf("modes = %+v; want one 1024x768", modes)
	}
	if current.Width != 1024 || current.Height != 768 {
		t.Errorf("current = %+v; want 1024x768", current)
	}
}

func TestParseXrandrQuery_NoConnectedOutput(t *testing.T) {
	const out = `Screen 0: minimum 320 x 200, current 0 x 0, maximum 16384 x 16384
Virtual-1 disconnected (normal left inverted right x axis y axis)`
	output, modes, _ := parseXrandrQuery(out)
	if output != "" || len(modes) != 0 {
		t.Errorf("disconnected-only: output=%q modes=%v; want empty", output, modes)
	}
}

func TestParseCvtModeline(t *testing.T) {
	const out = `# 1920x1080 59.96 Hz (CVT 2.07M9) hsync: 67.16 kHz; pclk: 173.00 MHz
Modeline "1920x1080_60.00"  173.00  1920 2048 2248 2576  1080 1083 1088 1125 -hsync +vsync`
	name, args, ok := parseCvtModeline(out)
	if !ok {
		t.Fatal("parseCvtModeline returned ok=false")
	}
	if name != "1920x1080_60.00" {
		t.Errorf("name = %q; want 1920x1080_60.00", name)
	}
	wantArgs := []string{"173.00", "1920", "2048", "2248", "2576", "1080", "1083", "1088", "1125", "-hsync", "+vsync"}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("args = %v; want %v", args, wantArgs)
	}
}

func TestParseCvtModeline_NoModeline(t *testing.T) {
	if _, _, ok := parseCvtModeline("# just a comment, no modeline\n"); ok {
		t.Error("parseCvtModeline should return ok=false when no Modeline line is present")
	}
}

func TestParseMutterCurrentState(t *testing.T) {
	// Representative gdbus rendering of org.gnome.Mutter.DisplayConfig
	// GetCurrentState for a single QEMU/stdvga (bochs-drm) monitor.
	const out = `(uint32 3, [(('Virtual-1', 'Red Hat', 'QEMU Monitor', '0x00000000'), ` +
		`[('1920x1080@60.000', 1920, 1080, 60.000004, 1.0, [1.0, 2.0], {'is-current': <true>, 'is-preferred': <true>}), ` +
		`('1280x1024@60.000', 1280, 1024, 60.0, 1.0, [1.0], {}), ` +
		`('1024x768@60.000', 1024, 768, 60.0, 1.0, [1.0], {})], ` +
		`{'is-builtin': <false>, 'display-name': <'QEMU Monitor'>}),], ` +
		`[(0, 0, 1.0, uint32 0, true, [('Virtual-1', {})], {})], ` +
		`{'layout-mode': <uint32 1>, 'supports-changing-layout-mode': <true>})`

	serial, connector, modes := parseMutterCurrentState(out)
	if serial != "3" {
		t.Errorf("serial = %q; want 3", serial)
	}
	if connector != "Virtual-1" {
		t.Errorf("connector = %q; want Virtual-1", connector)
	}
	if len(modes) != 3 {
		t.Fatalf("len(modes) = %d; want 3 (%+v)", len(modes), modes)
	}
	if modes[0].id != "1920x1080@60.000" || modes[0].w != 1920 || modes[0].h != 1080 {
		t.Errorf("modes[0] = %+v; want id=1920x1080@60.000 1920x1080", modes[0])
	}
	if !modes[0].current {
		t.Error("modes[0].current = false; want true (carries 'is-current': <true>)")
	}
	if modes[1].current || modes[2].current {
		t.Error("only modes[0] should be current")
	}
	// The connector tuple ('Virtual-1','Red Hat',...) must NOT be parsed as a mode.
	for _, m := range modes {
		if m.w == 0 || m.h == 0 {
			t.Errorf("zero-size mode leaked into list: %+v", m)
		}
	}
}

func TestParseMutterCurrentState_Empty(t *testing.T) {
	s, c, m := parseMutterCurrentState("()")
	if s != "" || c != "" || len(m) != 0 {
		t.Errorf("empty input should parse to zero values; got %q %q %+v", s, c, m)
	}
}

func TestPickLargest(t *testing.T) {
	// The exact list this VM's Mutter offered (no 1920x1080). Largest by area
	// is 1600x1200 (1.92M px) over 1680x1050 (1.76M px).
	modes := []Mode{
		{Width: 1680, Height: 1050}, {Width: 1600, Height: 1200},
		{Width: 1600, Height: 900}, {Width: 1280, Height: 1024},
		{Width: 1024, Height: 768}, {Width: 800, Height: 600},
	}
	got, err := pickLargest(modes)
	if err != nil {
		t.Fatalf("pickLargest err = %v", err)
	}
	if got.Width != 1600 || got.Height != 1200 {
		t.Errorf("pickLargest = %dx%d; want 1600x1200 (max area)", got.Width, got.Height)
	}
}

func TestPickLargest_AreaTiePrefersWider(t *testing.T) {
	// Equal area (both 2,073,600 px): the wider one wins.
	modes := []Mode{{Width: 1920, Height: 1080}, {Width: 1440, Height: 1440}}
	got, _ := pickLargest(modes)
	if got.Width != 1920 {
		t.Errorf("pickLargest tie = %dx%d; want the wider 1920x1080", got.Width, got.Height)
	}
}

func TestPickLargest_Empty(t *testing.T) {
	if _, err := pickLargest(nil); err == nil {
		t.Error("pickLargest(nil) should return ErrNoMatchingMode")
	}
	// Zero/negative dimensions are ignored.
	if _, err := pickLargest([]Mode{{Width: 0, Height: 0}}); err == nil {
		t.Error("pickLargest of only-invalid modes should return ErrNoMatchingMode")
	}
}
