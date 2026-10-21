//go:build ignore

// mksyso generates the Windows resource object (rsrc_windows_amd64.syso) that
// `go build` links into ahv-remote-console-agent.exe: the AHV Remote Console
// application icon plus version / publisher info. With these, the binary shows
// the brand icon in Explorer, the taskbar and the AutoPlay entry, and a
// friendly publisher name ("AHV Remote Console Guest Agent") in the UAC dialog.
//
// It is excluded from the normal build (//go:build ignore); the committed
// .syso is what the build actually links, so no resource tool is needed at
// build time. Re-run this ONLY when the icon or version changes:
//
//	go generate ./apps/ahv-remote-console-agent/cmd/ahv-remote-console-agent
//
// (or `go run apps/ahv-remote-console-agent/cmd/ahv-remote-console-agent/mksyso.go`).
package main

import (
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/tc-hib/winres"
	"github.com/tc-hib/winres/version"
)

// fileVersionFromEnv derives a Windows numeric version ("x.y.z.0") from the
// AHV_REMOTE_CONSOLE_VERSION env the Makefile exports (a git-describe string
// like "v1.2.3-5-gabc1234-dirty"). Only the leading "vX.Y.Z" is used; missing
// components default to 0. Falls back to "0.0.0.0" when the env is absent
// (e.g. a bare `go generate`) so a dev build is visibly un-versioned rather
// than masquerading as a release. The committed .syso reflects whatever
// version was set when `go generate` last ran — re-generate per release.
func fileVersionFromEnv() string {
	v := strings.TrimSpace(os.Getenv("AHV_REMOTE_CONSOLE_VERSION"))
	v = strings.TrimPrefix(v, "v")
	// Take the dotted numeric head: "1.2.3-5-gabc" → ["1","2","3-5-gabc"].
	parts := strings.SplitN(v, ".", 4)
	nums := []string{"0", "0", "0", "0"}
	for i := 0; i < 3 && i < len(parts); i++ {
		// Stop each component at the first non-digit (handles "3-5-gabc" → "3").
		d := parts[i]
		for j := 0; j < len(d); j++ {
			if d[j] < '0' || d[j] > '9' {
				d = d[:j]
				break
			}
		}
		if d == "" {
			break
		}
		if _, err := strconv.Atoi(d); err == nil {
			nums[i] = d
		}
	}
	return strings.Join(nums, ".")
}

func main() {
	// Resolve paths relative to THIS source file so it works from any cwd
	// (repo root or via go:generate in the package dir).
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		log.Fatal("mksyso: cannot resolve source path")
	}
	pkgDir := filepath.Dir(thisFile)
	repoRoot := filepath.Join(pkgDir, "..", "..", "..", "..")
	iconPath := filepath.Join(repoRoot, "build", "icons", "ahv-remote-console.ico")

	f, err := os.Open(iconPath)
	if err != nil {
		log.Fatalf("open icon %s: %v", iconPath, err)
	}
	icon, err := winres.LoadICO(f)
	_ = f.Close()
	if err != nil {
		log.Fatalf("load icon: %v", err)
	}

	rs := &winres.ResourceSet{}
	// ID(1) is the lowest numeric icon group, which the Windows shell uses as
	// the executable's display icon.
	if err := rs.SetIcon(winres.ID(1), icon); err != nil {
		log.Fatalf("set icon: %v", err)
	}

	vi := version.Info{}
	fv := fileVersionFromEnv()
	vi.SetFileVersion(fv)
	vi.SetProductVersion(fv)
	for k, v := range map[string]string{
		version.CompanyName:      "The AHV Remote Console Authors",
		version.ProductName:      "AHV Remote Console",
		version.FileDescription:  "AHV Remote Console Guest Agent",
		version.OriginalFilename: "ahv-remote-console-agent.exe",
		version.InternalName:     "ahv-remote-console-agent",
		version.LegalCopyright:   "The AHV Remote Console Authors",
	} {
		vi.Set(version.LangDefault, k, v)
	}
	rs.SetVersionInfo(vi)

	// asInvoker: the installer stays per-user (it self-elevates only the single
	// firmware sub-step). Common Controls v6 + per-monitor DPI keep the native
	// dialogs / WebView crisp.
	rs.SetManifest(winres.AppManifest{
		ExecutionLevel:      winres.AsInvoker,
		DPIAwareness:        winres.DPIPerMonitorV2,
		UseCommonControlsV6: true,
	})

	for _, t := range []struct {
		arch winres.Arch
		name string
	}{
		{winres.ArchAMD64, "rsrc_windows_amd64.syso"},
	} {
		out, err := os.Create(filepath.Join(pkgDir, t.name))
		if err != nil {
			log.Fatalf("create %s: %v", t.name, err)
		}
		if err := rs.WriteObject(out, t.arch); err != nil {
			_ = out.Close()
			log.Fatalf("write %s: %v", t.name, err)
		}
		_ = out.Close()
		log.Printf("wrote %s", t.name)
	}
}
