// Command isobuilder assembles the AHV Remote Console agent ISO.
//
// The output ISO is a hybrid ISO9660 + Joliet image meant to be uploaded
// to the Prism Image Service and attached as a CD-ROM to a guest VM.
// The operator then runs ahv-remote-console-agent.exe, whose graphical
// installer copies it in as the per-user resident agent.
//
// Inputs:
//   - A directory containing pre-built agent binaries with the filenames
//     ahv-remote-console-agent-<os>-<arch>[.exe] (produced by `make agent-bins`)
//     plus the Linux .rpm/.deb packages (produced by `make agent-pkgs`).
//   - A handful of static assets (README.txt, autorun.inf, linux/install.sh)
//     embedded via go:embed at build time.
//
// Output:
//   - A single .iso file ready for upload. Pure Go (no shell-out to
//     mkisofs / hdiutil / genisoimage), so it works on the macOS CI
//     runner without Homebrew.
package main

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
)

//go:embed assets/README.txt assets/autorun.inf assets/install.sh
var staticAssets embed.FS

const (
	// isoBlocksize is the ISO9660 logical block size. 2048 is the
	// canonical CD-ROM sector size and the only size that some older
	// readers (and a fair amount of Windows hypervisor tooling) can
	// reliably parse.
	isoBlocksize = 2048

	// volumeLabelDefault is used when the operator doesn't pass
	// -label on the command line.
	volumeLabelDefault = "AHV_AGENT"

	// volumeIDMax is the ISO9660 hard cap on the volume identifier
	// length. Per the spec this is 32 d-characters; we truncate
	// rather than refuse so a long version string doesn't fail the
	// build at the last step.
	volumeIDMax = 32
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "isobuilder: %v\n", err)
		os.Exit(1)
	}
}

type options struct {
	agentDir string
	out      string
	version  string
	label    string
	mtime    int64
}

func run(args []string) error {
	flags := flag.NewFlagSet("isobuilder", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var opts options
	flags.StringVar(&opts.agentDir, "agent-dir", "", "directory containing pre-built ahv-remote-console-agent-* binaries (required)")
	flags.StringVar(&opts.out, "out", "", "output ISO path (required)")
	flags.StringVar(&opts.version, "version", "", "version string embedded in the volume identifier (required)")
	flags.StringVar(&opts.label, "label", volumeLabelDefault, "volume label prefix")
	flags.Int64Var(&opts.mtime, "mtime", 0, "unix timestamp to normalize all ISO timestamps to (0 = leave "+
		"build-time stamps). Pass the git commit date for reproducible ISOs, so the "+
		"SHA-256-named catalog image dedupes across rebuilds of identical inputs.")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if opts.agentDir == "" {
		return errors.New("-agent-dir is required")
	}
	if opts.out == "" {
		return errors.New("-out is required")
	}
	if opts.version == "" {
		return errors.New("-version is required")
	}
	if opts.label == "" {
		opts.label = volumeLabelDefault
	}

	stagedFiles, agentBinCount, err := gatherStagedFiles(opts.agentDir)
	if err != nil {
		return err
	}
	if agentBinCount == 0 {
		return fmt.Errorf("no ahv-remote-console-agent-* binaries found under %s (run `make agent-bins` first)", opts.agentDir)
	}

	if err := buildISO(opts, stagedFiles); err != nil {
		// Best-effort cleanup of a half-written ISO so a re-run starts
		// from a clean slate (diskfs.Create refuses to overwrite).
		_ = os.Remove(opts.out)
		return err
	}
	if opts.mtime > 0 {
		// Must run before the sha256 below — the digest is the reproducibility
		// contract (it names the catalog image).
		if err := normalizeTimestamps(opts.out, time.Unix(opts.mtime, 0)); err != nil {
			_ = os.Remove(opts.out)
			return fmt.Errorf("normalize timestamps: %w", err)
		}
	}

	info, err := os.Stat(opts.out)
	if err != nil {
		return fmt.Errorf("stat output: %w", err)
	}
	digest, err := sha256File(opts.out)
	if err != nil {
		return fmt.Errorf("sha256 output: %w", err)
	}
	fmt.Printf("wrote %s (%s, %d bytes)\n", opts.out, humanBytes(info.Size()), info.Size())
	fmt.Printf("sha256 %s\n", digest)
	return nil
}

// stagedFile describes a single entry to write into the ISO.
type stagedFile struct {
	// isoPath is the path inside the ISO (POSIX-style, leading "/").
	isoPath string
	// source identifies where the bytes come from. Exactly one of
	// hostPath or embedded must be set.
	hostPath string
	embedded string
}

func (s stagedFile) open() (io.ReadCloser, int64, error) {
	if s.hostPath != "" {
		f, err := os.Open(s.hostPath)
		if err != nil {
			return nil, 0, err
		}
		st, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, 0, err
		}
		return f, st.Size(), nil
	}
	data, err := staticAssets.ReadFile(s.embedded)
	if err != nil {
		return nil, 0, fmt.Errorf("read embedded %s: %w", s.embedded, err)
	}
	// Embedded text assets are committed with LF line endings (the
	// repo's .gitattributes normalises everything to LF). Windows
	// tools generally tolerate LF, but historically some autoplay /
	// inf parsers expect CRLF, and Notepad pre-Win10 1809 will not
	// wrap LF-only files. Convert to CRLF at write time so the ISO
	// looks the way a Windows operator expects.
	if isWindowsTextAsset(s.isoPath) {
		data = toCRLF(data)
	}
	return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
}

// isWindowsTextAsset returns true for embedded assets whose ISO copy
// should use CRLF line endings (README.txt, autorun.inf). Agent
// binaries are explicitly NOT in this set — their on-disk bytes must
// be preserved exactly.
func isWindowsTextAsset(isoPath string) bool {
	switch strings.ToLower(filepath.Ext(isoPath)) {
	case ".cmd", ".bat", ".txt", ".inf", ".ps1":
		return true
	default:
		return false
	}
}

// toCRLF converts LF line endings to CRLF in-place. Existing CRLFs
// are left untouched (we detect and skip the CR-LF pair).
func toCRLF(in []byte) []byte {
	// Quick exit: if the input already contains a CR, assume it's
	// already CRLF-formatted and leave it alone. This avoids
	// double-CR-ing files that happened to be committed with native
	// Windows line endings (e.g. when a Windows contributor checked
	// them in).
	if bytes.Contains(in, []byte{'\r'}) {
		return in
	}
	out := make([]byte, 0, len(in)+bytes.Count(in, []byte{'\n'}))
	for _, b := range in {
		if b == '\n' {
			out = append(out, '\r', '\n')
		} else {
			out = append(out, b)
		}
	}
	return out
}

// gatherStagedFiles inspects agentDir and returns the set of files
// to write into the ISO, along with the count of agent binaries
// found. Naming convention from `make agent-bins`:
//   - ahv-remote-console-agent-windows-amd64.exe → /ahv-remote-console-agent.exe
//   - ahv-remote-console-agent-linux-*           → /linux/<original filename>
//
// Anything else under agentDir is ignored. The embedded static
// assets are appended at the end so they're always on the ISO; the
// agentBinCount return is the sentinel run() uses to reject "no
// binaries were staged" with a clear error.
func gatherStagedFiles(agentDir string) ([]stagedFile, int, error) {
	st, err := os.Stat(agentDir)
	if err != nil {
		return nil, 0, fmt.Errorf("stat agent-dir: %w", err)
	}
	if !st.IsDir() {
		return nil, 0, fmt.Errorf("agent-dir %s is not a directory", agentDir)
	}

	var out []stagedFile
	var agentBinCount int
	entries, err := os.ReadDir(agentDir)
	if err != nil {
		return nil, 0, fmt.Errorf("read agent-dir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		host := filepath.Join(agentDir, name)
		switch {
		case name == "ahv-remote-console-agent-windows-amd64.exe":
			// The unified Guest Agent — installer GUI when double-clicked,
			// resident daemon once installed. The thing the operator runs.
			out = append(out, stagedFile{isoPath: "/ahv-remote-console-agent.exe", hostPath: host})
			agentBinCount++
		case strings.HasPrefix(name, "ahv-remote-console-agent-linux-"):
			out = append(out, stagedFile{isoPath: "/linux/" + name, hostPath: host})
			agentBinCount++
		case name == "ahv-remote-console-agent.rpm" || name == "ahv-remote-console-agent.deb":
			// Distro packages (`make agent-pkgs`) — the desktop-Linux install
			// path: double-click in Files → GNOME Software/Discover → Install →
			// "Agent Setup". Same binary as the raw linux file above, wrapped;
			// not counted as an agent binary (the raw file is the sentinel).
			out = append(out, stagedFile{isoPath: "/linux/" + name, hostPath: host})
		default:
			// Unrecognised; skip silently. Anything that matters is
			// covered by the cases above; ignoring the rest keeps
			// the ISO small.
		}
	}

	// Embedded assets: README/autorun at the ISO root, the manual-install
	// fallback script next to the Linux binary. install.sh has a .sh
	// extension, so isWindowsTextAsset leaves its LF endings alone.
	out = append(out,
		stagedFile{isoPath: "/README.txt", embedded: "assets/README.txt"},
		stagedFile{isoPath: "/autorun.inf", embedded: "assets/autorun.inf"},
		stagedFile{isoPath: "/linux/install.sh", embedded: "assets/install.sh"},
	)
	return out, agentBinCount, nil
}

// buildISO performs the actual ISO9660 + Joliet write. The output is
// truncated to the exact number of sectors used by Finalize so we
// don't ship a 50MB file with 40MB of trailing zeroes.
func buildISO(opts options, files []stagedFile) error {
	// Pre-allocate generously; we trim back to the actual used size
	// once Finalize tells us how many sectors it consumed. The
	// underlying file is created sparse on the macOS/Linux APFS/ext4
	// filesystems we run on so the temporary over-allocation costs
	// effectively nothing.
	diskSize, err := estimateDiskSize(files)
	if err != nil {
		return err
	}

	// diskfs.Create refuses to overwrite an existing file, so clear
	// any stale output first. Re-running `make iso` should always
	// "just work".
	if err := os.Remove(opts.out); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove old output: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(opts.out), 0o755); err != nil {
		return fmt.Errorf("mkdir output dir: %w", err)
	}

	mydisk, err := diskfs.Create(opts.out, diskSize, diskfs.SectorSizeDefault)
	if err != nil {
		return fmt.Errorf("create disk image: %w", err)
	}
	// ISO9660 only accepts 2048/4096/8192-byte logical blocks; the
	// default sector size on diskfs is 512, which would silently
	// produce a malformed image.
	mydisk.LogicalBlocksize = isoBlocksize

	volumeID := buildVolumeID(opts.label, opts.version)
	spec := disk.FilesystemSpec{
		Partition:   0,
		FSType:      filesystem.TypeISO9660,
		VolumeLabel: volumeID,
	}
	fsys, err := mydisk.CreateFilesystem(spec)
	if err != nil {
		return fmt.Errorf("create iso9660 filesystem: %w", err)
	}

	// Walk the staged files, mkdir-p the parent for each, and stream
	// the bytes in.
	dirsCreated := map[string]struct{}{}
	for _, sf := range files {
		dir := filepath.ToSlash(filepath.Dir(sf.isoPath))
		if dir != "/" && dir != "." && dir != "" {
			if _, ok := dirsCreated[dir]; !ok {
				if err := fsys.Mkdir(dir); err != nil {
					return fmt.Errorf("mkdir %s in iso: %w", dir, err)
				}
				dirsCreated[dir] = struct{}{}
			}
		}
		if err := writeOne(fsys, sf); err != nil {
			return err
		}
	}

	iso, ok := fsys.(*iso9660.FileSystem)
	if !ok {
		return fmt.Errorf("expected *iso9660.FileSystem, got %T", fsys)
	}

	// RockRidge PX (POSIX-mode) entries are derived from the staging
	// workspace's on-disk modes at Finalize time, and files are staged
	// without exec bits. Mark the Linux payloads executable so an
	// exec-allowed loop mount lets `./install.sh` (and the raw binary)
	// run directly. noexec automounts still can't exec in place — the
	// documented `sh install.sh` path covers those.
	for _, sf := range files {
		if strings.HasSuffix(sf.isoPath, ".sh") ||
			strings.HasPrefix(sf.isoPath, "/linux/ahv-remote-console-agent-linux-") {
			ws := filepath.Join(iso.Workspace(), filepath.FromSlash(sf.isoPath))
			if err := os.Chmod(ws, 0o755); err != nil {
				return fmt.Errorf("chmod +x %s in workspace: %w", sf.isoPath, err)
			}
		}
	}

	finalizeOpts := iso9660.FinalizeOptions{
		// Joliet provides UCS-2 long filenames in the Supplementary
		// Volume Descriptor; modern Windows + macOS + most Linux
		// distros prefer that to the 8.3 ISO9660 table.
		Joliet: true,
		// RockRidge gives us POSIX attributes on Linux. The agent
		// payload doesn't need them, but enabling RR is essentially
		// free and lets `mount -o loop` show sensible permissions.
		RockRidge:           true,
		VolumeIdentifier:    volumeID,
		PublisherIdentifier: "AHV REMOTE CONSOLE",
	}
	if err := iso.Finalize(finalizeOpts); err != nil {
		return fmt.Errorf("finalize iso: %w", err)
	}

	// Trim back to the actual used size. diskfs leaves the file at
	// the preallocated size; for a 50MB preallocation with a 15MB
	// payload that's 35MB of trailing zeroes we'd otherwise ship.
	if err := trimToUsedSize(opts.out); err != nil {
		return fmt.Errorf("trim iso to used size: %w", err)
	}
	return nil
}

// writeOne streams a single file into the open ISO filesystem. We
// stream rather than read-all-then-write so we don't hold the full
// (~5-10MB) agent binary in memory.
func writeOne(fsys filesystem.FileSystem, sf stagedFile) error {
	src, _, err := sf.open()
	if err != nil {
		return fmt.Errorf("open source for %s: %w", sf.isoPath, err)
	}
	defer src.Close()

	dst, err := fsys.OpenFile(sf.isoPath, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return fmt.Errorf("create %s in iso: %w", sf.isoPath, err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("copy bytes for %s: %w", sf.isoPath, err)
	}
	return nil
}

// estimateDiskSize returns a generous upper bound on the on-disk size
// of the resulting ISO. Slightly over-allocating is cheap (the file
// is sparse during write and trimmed back at the end); under-
// allocating causes Finalize to refuse with "out of space".
func estimateDiskSize(files []stagedFile) (int64, error) {
	// Fixed overhead: 16-sector system area + ~16 sectors of volume
	// descriptors + ~32 sectors for path tables + ~64 sectors for
	// directory records. 128 sectors = 256KB is more than enough.
	const overheadSectors int64 = 256
	var payloadSectors int64
	for _, sf := range files {
		sz, err := sizeOf(sf)
		if err != nil {
			return 0, err
		}
		// Each file is rounded up to the next 2K sector. Plus a
		// safety pad of 1 sector per file for ISO header data each
		// file carries.
		payloadSectors += (sz+isoBlocksize-1)/isoBlocksize + 1
	}
	total := (overheadSectors + payloadSectors) * isoBlocksize
	// Floor at 1MB so very tiny test inputs still get a usable ISO.
	if total < 1<<20 {
		total = 1 << 20
	}
	// Add 25% headroom in case Joliet / Rock Ridge inflate the
	// directory area unexpectedly.
	total += total / 4
	return total, nil
}

func sizeOf(sf stagedFile) (int64, error) {
	if sf.hostPath != "" {
		st, err := os.Stat(sf.hostPath)
		if err != nil {
			return 0, fmt.Errorf("stat %s: %w", sf.hostPath, err)
		}
		return st.Size(), nil
	}
	data, err := fs.ReadFile(staticAssets, sf.embedded)
	if err != nil {
		return 0, fmt.Errorf("read embedded %s: %w", sf.embedded, err)
	}
	// For text assets we expand LF→CRLF at write time, so the size
	// estimate has to match what we'll actually write.
	if isWindowsTextAsset(sf.isoPath) {
		data = toCRLF(data)
	}
	return int64(len(data)), nil
}

// trimToUsedSize reads the PVD from the finalized ISO and truncates
// the file to exactly the number of sectors the filesystem actually
// uses. This avoids shipping a multi-megabyte tail of zeroes.
//
// The volume space size lives at offset 80 of the PVD (which itself
// starts at sector 16 = byte offset 32768). It's an 8-byte
// "both-endian" 32-bit value: little-endian first, then big-endian.
// We only read the little-endian half.
func trimToUsedSize(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()

	const pvdOffset = 16 * isoBlocksize // sector 16
	const volSpaceOffsetInPVD = 80      // per ISO9660 spec
	buf := make([]byte, 4)
	if _, err := f.ReadAt(buf, pvdOffset+volSpaceOffsetInPVD); err != nil {
		return fmt.Errorf("read PVD volume size: %w", err)
	}
	sectors := int64(buf[0]) | int64(buf[1])<<8 | int64(buf[2])<<16 | int64(buf[3])<<24
	if sectors <= 0 {
		// Defensive: leave the file alone if the PVD looks bogus.
		// That's a sign Finalize didn't write a valid PVD, in which
		// case the ISO is broken regardless of size.
		return fmt.Errorf("PVD reports %d sectors; refusing to truncate", sectors)
	}
	want := sectors * isoBlocksize
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() <= want {
		// Already at or under the reported volume size; nothing to do.
		return nil
	}
	if err := f.Truncate(want); err != nil {
		return fmt.Errorf("truncate to %d: %w", want, err)
	}
	return nil
}

// buildVolumeID joins the label + version into a volume identifier
// that fits the ISO9660 32-character limit. The identifier is
// uppercased and stripped of characters outside the d-character set
// (A-Z, 0-9, _) so it round-trips through every ISO reader.
func buildVolumeID(label, version string) string {
	v := sanitizeDChar(version)
	l := sanitizeDChar(label)
	id := l + "_" + v
	if len(id) > volumeIDMax {
		id = id[:volumeIDMax]
	}
	return id
}

// sanitizeDChar maps any character outside [A-Z0-9_] to "_" and
// uppercases the rest. Hyphens, dots, and lowercase letters all show
// up in our version strings (e.g. "0.0.0-dev"); without this step the
// resulting volume ID would be rejected by stricter ISO9660 readers.
func sanitizeDChar(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
			out = append(out, c)
		case c >= 'a' && c <= 'z':
			out = append(out, c-32)
		case c >= '0' && c <= '9':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func humanBytes(n int64) string {
	const (
		KiB = 1024
		MiB = 1024 * KiB
		GiB = 1024 * MiB
	)
	switch {
	case n >= GiB:
		return fmt.Sprintf("%.2f GiB", float64(n)/float64(GiB))
	case n >= MiB:
		return fmt.Sprintf("%.2f MiB", float64(n)/float64(MiB))
	case n >= KiB:
		return fmt.Sprintf("%.2f KiB", float64(n)/float64(KiB))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
