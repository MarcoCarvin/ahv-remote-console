#!/bin/sh
# AHV Remote Console agent - manual installer for Linux guests.
#
# Desktop guests have a friendlier path: double-click the package for your
# distribution in this folder (ahv-remote-console-agent.rpm for RHEL/Rocky/
# Fedora, .deb for Ubuntu/Debian), click Install, then launch
# "AHV Remote Console Agent Setup".
#
# This script is the fallback for headless guests or users without admin
# rights: it installs the agent for YOUR USER ONLY (no root) and enables the
# per-user autostart.
#
# Usage:  sh install.sh [none|WxH|max]
#         Standard guest resolution applied at each login. Default "max" =
#         the largest mode the display offers (safe on Linux; xrandr/gdbus apply
#         it live). A WxH (e.g. 1920x1080) forces that exact mode; "none" leaves
#         the resolution unchanged.

set -eu

dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
bin="$dir/ahv-remote-console-agent-linux-amd64"
if [ ! -f "$bin" ]; then
    echo "error: $bin not found - run this script from the mounted agent ISO" >&2
    exit 1
fi

# The ISO is mounted read-only (and often noexec), so the binary cannot run
# in place. Stage an executable copy in the first directory that allows
# execution — hardened guests commonly mount /tmp noexec, so probe a few
# candidates with `--version` before committing. --install then copies the
# agent into ~/.local/bin and the staged copy is removed.
tmp=
trap '[ -n "$tmp" ] && rm -f "$tmp"' EXIT
for cand in "${XDG_RUNTIME_DIR:-}" "${TMPDIR:-}" "$HOME/.cache" /tmp; do
    [ -n "$cand" ] || continue
    mkdir -p "$cand" 2>/dev/null || continue
    t=$(mktemp "$cand/ahv-remote-console-agent.XXXXXX" 2>/dev/null) || continue
    if ! cp "$bin" "$t" 2>/dev/null || ! chmod 0755 "$t" 2>/dev/null; then
        rm -f "$t"
        continue
    fi
    if "$t" --version >/dev/null 2>&1; then
        tmp=$t
        break
    fi
    rm -f "$t" # execution refused (noexec mount?) - try the next candidate
done
if [ -z "$tmp" ]; then
    echo "error: no staging directory allows execution (noexec mounts?)." >&2
    echo "Copy the agent binary somewhere writable and executable, then run:" >&2
    echo "    cp $bin ~/agent && chmod +x ~/agent && ~/agent --install" >&2
    exit 1
fi

"$tmp" --install --default-resolution="${1:-max}"

echo ""
echo "Installed. The agent is running and starts at every login"
echo "(systemd user unit: ahv-remote-console-agent)."

# Clipboard prerequisites the per-user install cannot fix by itself.

# The clipboard bridge shells out to a guest clipboard helper. Without one the
# agent still runs (and pins the display resolution) but clipboard sync is off.
# The .deb/.rpm packages Recommend these; a per-user install must add them by hand.
if ! command -v wl-copy >/dev/null 2>&1 \
    && ! command -v xclip >/dev/null 2>&1 \
    && ! command -v xsel >/dev/null 2>&1; then
    echo ""
    echo "NOTE: no clipboard helper found — clipboard sync is disabled until you"
    echo "install one and restart the agent:"
    echo "    # RHEL/AlmaLinux/Rocky (wl-clipboard is in EPEL, not the base repos):"
    echo "    sudo dnf install epel-release && sudo dnf install wl-clipboard"
    echo "    # Debian/Ubuntu:        sudo apt install wl-clipboard"
    echo "    # X11 sessions instead: install xclip or xsel"
    echo "    systemctl --user restart ahv-remote-console-agent"
fi

port=/dev/ttyS2
if [ ! -e "$port" ]; then
    echo ""
    echo "NOTE: the clipboard serial port ($port) is not present yet."
    echo "In AHV Remote Console: power this VM fully OFF, add the serial"
    echo "port, then power it back ON (an in-guest reboot is NOT enough)."
elif [ ! -w "$port" ] || [ ! -r "$port" ]; then
    grp=$(stat -c %G "$port" 2>/dev/null || echo dialout)
    echo ""
    echo "NOTE: your user cannot access $port (group: $grp). Run:"
    echo "    sudo usermod -aG $grp ${USER:-$(id -un)}"
    echo "then log out and back in for the clipboard to start working."
fi
