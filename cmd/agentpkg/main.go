// Command agentpkg wraps the static Linux agent binary in distro packages
// (.rpm + .deb) so a desktop-Linux guest can install it the GNOME-native way:
// double-click the package in Files → GNOME Software/Discover opens → Install
// (polkit prompt) → Launch. GNOME deliberately refuses to execute raw binaries
// from removable media, so the package manager is the only sanctioned
// double-click install channel — this is the Linux counterpart of the agent's
// Windows GUI installer.
//
// The packages deliberately contain NO machine-wide autostart: two users'
// daemons would contend for the VM's single vdagent serial port, and the
// agent's autostart is per-user by design. Instead they ship:
//
//   - /usr/bin/ahv-remote-console-agent          (the static binary)
//   - /usr/share/applications/…-setup.desktop    ("Agent Setup" launcher —
//     the post-install "Launch" button in GNOME Software, and an app-grid
//     entry; runs `--setup`, which enables the per-user systemd unit)
//   - /usr/share/icons/hicolor/256x256/apps/….png
//
// Built with nfpm (pure Go), so packaging cross-compiles from any host with
// no rpm/dpkg toolchain. x86-64 only — AHV guests are x86-64.
package main

import (
	_ "embed"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/goreleaser/nfpm/v2"
	"github.com/goreleaser/nfpm/v2/files"

	_ "github.com/goreleaser/nfpm/v2/deb" // register the deb packager
	_ "github.com/goreleaser/nfpm/v2/rpm" // register the rpm packager
)

//go:embed assets/ahv-remote-console-agent-setup.desktop
var setupDesktop []byte

const pkgName = "ahv-remote-console-agent"

func main() {
	var (
		binPath  = flag.String("bin", "", "path to the linux/amd64 agent binary (required)")
		iconPath = flag.String("icon", "", "path to a 256x256 PNG icon (required)")
		outDir   = flag.String("out", "", "directory to write the .rpm and .deb into (required)")
		version  = flag.String("version", "dev", "package version (git describe; sanitized per format)")
		mtime    = flag.Int64("mtime", 0, "unix timestamp for package metadata (rpm buildtime, tar mtimes); "+
			"0 = now. Pass the git commit date for reproducible packages — identical "+
			"inputs then yield identical bytes, so the ISO's SHA-256-named catalog "+
			"image dedupes across rebuilds instead of re-uploading.")
	)
	flag.Parse()
	if *binPath == "" || *iconPath == "" || *outDir == "" {
		flag.Usage()
		os.Exit(2)
	}
	var stamp time.Time
	if *mtime > 0 {
		stamp = time.Unix(*mtime, 0).UTC()
	}

	// The .desktop launcher is embedded, but nfpm wants files on disk —
	// stage it in a temp dir.
	tmp, err := os.MkdirTemp("", "agentpkg-*")
	if err != nil {
		log.Fatalf("agentpkg: tempdir: %v", err)
	}
	defer os.RemoveAll(tmp)
	desktopSrc := filepath.Join(tmp, pkgName+"-setup.desktop")
	if err := os.WriteFile(desktopSrc, setupDesktop, 0o644); err != nil {
		log.Fatalf("agentpkg: stage desktop file: %v", err)
	}

	for _, format := range []string{"rpm", "deb"} {
		out := filepath.Join(*outDir, pkgName+"."+format)
		if err := buildPackage(format, *binPath, *iconPath, desktopSrc, *version, stamp, out); err != nil {
			log.Fatalf("agentpkg: %s: %v", format, err)
		}
		fmt.Println("wrote", out)
	}
}

func buildPackage(format, binPath, iconPath, desktopSrc, version string, stamp time.Time, out string) error {
	info := &nfpm.Info{
		Name:        pkgName,
		Arch:        "amd64", // nfpm translates to x86_64 for rpm
		Platform:    "linux",
		Version:     packageVersion(version),
		Maintainer:  "AHV Remote Console maintainers <maintainers@example.invalid>",
		Description: "AHV Remote Console guest agent: clipboard sync and display sizing for this VM.\n  Run \"Agent Setup\" after installing to enable it for your user.",
		Homepage:    "https://github.com/MarcoCarvin/ahv-remote-console",
		License:     "Apache-2.0",
		// Deterministic metadata timestamp (see the -mtime flag). Zero means
		// "now" — nfpm's default, non-reproducible.
		MTime: stamp,
		Overridables: nfpm.Overridables{
			// The guest<->host clipboard bridge shells out to a clipboard
			// helper: wl-clipboard on Wayland, xclip/xsel on X11. Recommend
			// (not hard-Depend) both so apt/dnf pull them by default while a
			// display-only or minimal guest can still install — the agent now
			// degrades gracefully if none is present (clipboard disabled, the
			// rest runs). Which one is needed depends on the guest's session
			// type, so neither can be a single hard dependency.
			Recommends: []string{"wl-clipboard", "xclip"},
			Contents: files.Contents{
				{
					Source:      binPath,
					Destination: "/usr/bin/" + pkgName,
					FileInfo:    &files.ContentFileInfo{Mode: 0o755, MTime: stamp},
				},
				{
					Source:      desktopSrc,
					Destination: "/usr/share/applications/" + pkgName + "-setup.desktop",
					FileInfo:    &files.ContentFileInfo{Mode: 0o644, MTime: stamp},
				},
				{
					Source:      iconPath,
					Destination: "/usr/share/icons/hicolor/256x256/apps/" + pkgName + ".png",
					FileInfo:    &files.ContentFileInfo{Mode: 0o644, MTime: stamp},
				},
			},
		},
	}

	pkger, err := nfpm.Get(format)
	if err != nil {
		return err
	}
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := pkger.Package(info, f); err != nil {
		return err
	}
	return f.Close()
}

// packageVersion sanitizes a git-describe string ("v0.4.0-12-gabc1234",
// "stable-request-pump-6-gd354f74-dirty", "dev") into a version both rpm and
// deb accept: leading "v" dropped, every "-" replaced with "." (rpm forbids
// dashes in the version field), empty segments collapsed, and — because
// Debian policy requires the version to BEGIN WITH A DIGIT (a letter-leading
// version sorts before every numeric one and breaks upgrade ordering) — a
// "0.0.0." prefix when the tag isn't numeric. "dev" builds become "0.0.0.dev".
func packageVersion(v string) string {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if v == "" || v == "dev" {
		return "0.0.0.dev"
	}
	v = strings.ReplaceAll(v, "-", ".")
	// Strip anything neither rpm nor deb tolerates (keep [A-Za-z0-9.+~]),
	// then drop the empty segments stripping can leave behind.
	v = regexp.MustCompile(`[^A-Za-z0-9.+~]+`).ReplaceAllString(v, ".")
	v = strings.Trim(regexp.MustCompile(`\.{2,}`).ReplaceAllString(v, "."), ".")
	if v == "" {
		return "0.0.0.dev"
	}
	if v[0] < '0' || v[0] > '9' {
		return "0.0.0." + v
	}
	return v
}
