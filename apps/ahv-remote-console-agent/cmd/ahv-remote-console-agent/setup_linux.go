//go:build linux

// Dialog-driven setup for Linux desktop guests — the Linux counterpart of the
// Windows GUI installer. GNOME refuses to execute raw binaries from removable
// media, so desktop guests install the agent the distro-native way: the
// .rpm/.deb on the agent ISO places this binary at /usr/bin plus an
// "Agent Setup" launcher, and that launcher runs `--setup`. This mode wraps the
// existing per-user --install in confirmation/resolution dialogs and ends with
// the two pieces of guidance the clipboard actually depends on (serial-port
// group access, COM3 power-off).
//
// Dialogs use zenity (GNOME) or kdialog (KDE) — whichever is on PATH — with a
// plain-terminal fallback so `--setup` also works over SSH. No toolkit is
// linked: the agent stays a static CGO-free binary; dialog tools are exec'd.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"syscall"
)

const setupTitle = "AHV Remote Console Agent"

// Resolution choices offered by the picker. "none" (the flag default) is
// pre-selected and leaves the resolution unchanged — the safe choice, since a
// runtime resize can disrupt the operator console on AHV std-VGA. Specific WxH
// values force that exact mode (operator-verified); "max" picks the largest
// offered mode (opt-in; may exceed what the virtual GPU can display).
var setupResolutions = []string{"none", "1920x1080", "2560x1440", "3840x2160", "max"}

func runSetupMode(f flags) error {
	ui := pickSetupUI()
	if ui == nil {
		return errors.New("--setup needs zenity or kdialog (or a terminal); run --install for the non-interactive install")
	}

	username := "this user"
	if u, err := user.Current(); err == nil && u.Username != "" {
		username = u.Username
	}

	if !ui.confirm(fmt.Sprintf(
		"Install the AHV Remote Console agent for %s?\n\n"+
			"It enables clipboard sync with the AHV Remote Console app, starts at "+
			"each login, and bridges only this user's session.", username)) {
		return nil
	}

	res := ui.pickResolution(f.defaultResolution)
	if err := doInstall(res); err != nil {
		ui.showError("Install failed:\n\n" + err.Error())
		return err
	}

	notes := setupSerialNotes(ui, username)
	msg := "The agent is installed and running. It starts automatically at every login."
	if notes != "" {
		msg += "\n\n" + notes
	}
	ui.showInfo(msg)
	return nil
}

// setupSerialNotes checks the one thing the freshly-installed daemon cannot
// fix itself — access to the vdagent serial port — and returns operator
// guidance for the final dialog. Deliberately stat-based: opening the port
// here could disturb the daemon that systemd just started.
func setupSerialNotes(ui *setupUI, username string) string {
	fi, err := os.Stat(defaultSerialPort)
	if err != nil {
		// No COM3 yet: the AHV side hasn't added the serial port. The daemon
		// retries on its own once it appears.
		return fmt.Sprintf(
			"Note: the clipboard serial port (%s) is not present yet. In AHV Remote "+
				"Console, power this VM fully OFF, add the serial port, then power it "+
				"back ON — an in-guest reboot is not enough.", defaultSerialPort)
	}
	grp, ok := serialGroupToJoin(fi)
	if !ok {
		return "" // port exists and is accessible — nothing to flag
	}
	fixCmd := fmt.Sprintf("sudo usermod -aG %s %s", grp, username)
	if ui.confirm(fmt.Sprintf(
		"Your user cannot access the clipboard serial port (%s, group %q).\n\n"+
			"Add %s to the %q group now? You will be asked for an administrator "+
			"password, and the change takes effect at your next login.",
		defaultSerialPort, grp, username, grp)) {
		if err := exec.Command("pkexec", "usermod", "-aG", grp, username).Run(); err == nil {
			return "Serial-port access granted — log out and back in for the clipboard to start working."
		}
	}
	return fmt.Sprintf(
		"To finish, grant serial-port access and re-login:\n\n    %s", fixCmd)
}

// serialGroupToJoin reports the group that owns the serial port when (and only
// when) joining it is what would grant the current user access. Returns
// ok=false when the user can already read+write the device (owner, group
// member, world-rw, or root) or when group lookup fails.
func serialGroupToJoin(fi os.FileInfo) (string, bool) {
	st, okSys := fi.Sys().(*syscall.Stat_t)
	u, err := user.Current()
	if !okSys || err != nil {
		return "", false
	}
	if u.Uid == "0" {
		return "", false
	}
	mode := fi.Mode().Perm()
	if mode&0o006 == 0o006 { // world rw
		return "", false
	}
	if fmt.Sprint(st.Uid) == u.Uid && mode&0o600 == 0o600 { // owner rw
		return "", false
	}
	gids, err := u.GroupIds()
	if err != nil {
		return "", false
	}
	devGid := fmt.Sprint(st.Gid)
	for _, gid := range gids {
		if gid == devGid {
			return "", false // already in the device's group
		}
	}
	if mode&0o060 != 0o060 {
		return "", false // joining the group would not grant rw anyway
	}
	g, err := user.LookupGroupId(devGid)
	if err != nil {
		return "", false
	}
	return g.Name, true
}

// ---- dialog plumbing ----

// setupUI runs the dialogs through one of three backends: zenity, kdialog, or
// plain terminal prompts. nil when none is usable (no dialog tool on PATH and
// stdin is not a terminal — e.g. launched from a .desktop on a system without
// zenity; the error surfaces in the journal).
type setupUI struct {
	tool string // "zenity" | "kdialog" | "tty"
}

func pickSetupUI() *setupUI {
	for _, tool := range []string{"zenity", "kdialog"} {
		if _, err := exec.LookPath(tool); err == nil {
			return &setupUI{tool: tool}
		}
	}
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		return &setupUI{tool: "tty"}
	}
	return nil
}

func (ui *setupUI) confirm(text string) bool {
	switch ui.tool {
	case "zenity":
		return exec.Command("zenity", "--question", "--title", setupTitle,
			"--no-wrap", "--text", text).Run() == nil
	case "kdialog":
		return exec.Command("kdialog", "--title", setupTitle, "--yesno", text).Run() == nil
	default:
		fmt.Println(text)
		fmt.Print("Proceed? [y/N] ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		line = strings.ToLower(strings.TrimSpace(line))
		return line == "y" || line == "yes"
	}
}

// pickResolution returns the standard guest resolution to bake into the
// autostart unit. A closed/cancelled dialog falls back to def — cancelling the
// picker shouldn't abort an install the operator already confirmed.
//
// EVERY branch validates the picked value before returning it: the result
// feeds doInstall, which bakes it verbatim into the systemd unit's ExecStart
// — the exact spawn sink parseFlags guards on the flag path (see its
// validation comment). A misbehaving dialog tool (zenity's radiolist output
// is version-dependent) must degrade to def, not crash-loop the daemon by
// persisting a token its own parseFlags will reject at every login.
func (ui *setupUI) pickResolution(def string) string {
	switch ui.tool {
	case "zenity":
		args := []string{"--list", "--radiolist", "--title", setupTitle,
			"--text", "Standard guest resolution at each login ('max' = largest the display offers):",
			"--column", "", "--column", "Resolution", "--hide-header"}
		for _, r := range setupResolutions {
			sel := "FALSE"
			if r == def {
				sel = "TRUE"
			}
			args = append(args, sel, r)
		}
		out, err := exec.Command("zenity", args...).Output()
		if pick := strings.TrimSpace(string(out)); err == nil && validResolution(pick) {
			return pick
		}
		return def
	case "kdialog":
		args := []string{"--title", setupTitle, "--radiolist",
			"Standard guest resolution at each login ('max' = largest the display offers):"}
		for _, r := range setupResolutions {
			sel := "off"
			if r == def {
				sel = "on"
			}
			args = append(args, r, r, sel)
		}
		out, err := exec.Command("kdialog", args...).Output()
		if pick := strings.TrimSpace(string(out)); err == nil && validResolution(pick) {
			return pick
		}
		return def
	default:
		fmt.Printf("Standard guest resolution %v [%s]: ", setupResolutions, def)
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if pick := strings.TrimSpace(line); pick != "" {
			if validResolution(pick) {
				return pick
			}
			fmt.Println("unrecognized resolution, keeping", def)
		}
		return def
	}
}

// validResolution accepts exactly what parseFlags accepts for
// --default-resolution: a bare "WxH" token, the "max"/"largest" sentinel, or
// the "none" opt-out.
func validResolution(s string) bool {
	if _, _, ok := parseResolution(s); ok {
		return true
	}
	return strings.EqualFold(s, "none") || strings.EqualFold(s, "max") || strings.EqualFold(s, "largest")
}

func (ui *setupUI) showInfo(text string)  { ui.message(text, false) }
func (ui *setupUI) showError(text string) { ui.message(text, true) }

func (ui *setupUI) message(text string, isErr bool) {
	switch ui.tool {
	case "zenity":
		kind := "--info"
		if isErr {
			kind = "--error"
		}
		_ = exec.Command("zenity", kind, "--title", setupTitle, "--no-wrap", "--text", text).Run()
	case "kdialog":
		kind := "--msgbox"
		if isErr {
			kind = "--error"
		}
		_ = exec.Command("kdialog", "--title", setupTitle, kind, text).Run()
	default:
		w := os.Stdout
		if isErr {
			w = os.Stderr
		}
		fmt.Fprintln(w, text)
	}
}
