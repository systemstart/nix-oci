package oci

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// LayerResult carries the two digests of one layer. They are not
// interchangeable: DiffID names the *uncompressed* tar and goes into the image
// config's rootfs.diff_ids, while Digest names the *compressed* blob and goes
// into the manifest's layer descriptor. Conflating them produces an image that
// every runtime rejects.
type LayerResult struct {
	DiffID string
	Digest string
	Size   int64 // size of the compressed blob
}

// epochTime is the mtime stamped on every tar entry.
var epochTime = time.Unix(0, 0).UTC()

// ancestorDirMode is forced onto the directories above the roots rather than
// read from the host. The real /nix/store is commonly mode 1775 and its mode
// varies between machines, so inheriting it would make the layer digest
// host-dependent.
const ancestorDirMode = 0o755

// ancestorsOf returns every parent directory of the roots, deduplicated.
// A layer must carry them, because a tar that contains nix/store/foo/ without
// nix/ and nix/store/ leaves their permissions up to the runtime.
func ancestorsOf(roots []string) []string {
	seen := make(map[string]bool)
	var dirs []string

	for _, root := range roots {
		for dir := path.Dir(path.Clean(filepath.ToSlash(root))); dir != "/" && dir != "."; dir = path.Dir(dir) {
			if !seen[dir] {
				seen[dir] = true
				dirs = append(dirs, dir)
			}
		}
	}

	return dirs
}

// WriteLayer writes a deterministic tar+gzip layer covering roots (a list of
// store paths) to w, and returns both digests. Entries keep their absolute store
// locations (e.g. /nix/store/foo -> nix/store/foo).
func WriteLayer(w io.Writer, roots []string) (LayerResult, error) {
	return writeLayerStream(w, func(tw *tar.Writer) error { return writeEntries(tw, roots) })
}

// OwnershipRule assigns a uid/gid to a customization-layer entry. Path is
// root-relative (e.g. "home/nonroot"); Recursive extends the rule to everything
// beneath it. Rules only apply to the customization layer -- store-path layers
// are always root-owned -- and are the deterministic stand-in for dockerTools'
// fakeRootCommands/chown (no fakeroot, ownership is stated, not observed).
type OwnershipRule struct {
	Path      string
	UID, GID  int
	Recursive bool
}

// WriteRootedLayer writes a deterministic tar+gzip layer from the *contents* of
// srcDir, mapped to the image root (srcDir/etc/passwd -> etc/passwd). This is
// how the customization layer carries non-store content: /etc scaffolding,
// /tmp, injected files. Entries default to uid/gid 0 like every other layer;
// any matching rule in own overrides that, so selected paths can be non-root.
func WriteRootedLayer(w io.Writer, srcDir string, own []OwnershipRule) (LayerResult, error) {
	return writeLayerStream(w, func(tw *tar.Writer) error { return writeRootedEntries(tw, srcDir, own) })
}

// cleanOwnName normalizes a rule path or tar name to a bare root-relative form
// (no leading "./" or "/", no trailing "/") so the two compare directly.
func cleanOwnName(s string) string {
	return strings.Trim(strings.TrimPrefix(s, "./"), "/")
}

// applyOwnership reassigns a header's uid/gid if any ownership rule matches,
// leaving the forced-root default in place otherwise.
func applyOwnership(header *tar.Header, own []OwnershipRule) {
	if uid, gid, ok := ownerFor(own, header.Name); ok {
		header.Uid, header.Gid = uid, gid
	}
}

// ownerFor returns the uid/gid a rule assigns to the tar entry named name, and
// whether any rule matched. Later rules win, so a recursive parent can be listed
// first and a specific child override it.
func ownerFor(rules []OwnershipRule, name string) (uid, gid int, ok bool) {
	n := cleanOwnName(name)
	for _, r := range rules {
		p := cleanOwnName(r.Path)
		if n == p || (r.Recursive && strings.HasPrefix(n, p+"/")) {
			uid, gid, ok = r.UID, r.GID, true
		}
	}

	return uid, gid, ok
}

// writeLayerStream sets up the tar+gzip pipeline, hashes it twice in a single
// pass -- a tee before compression and a tee after it, so layer content is never
// buffered or read twice (see DESIGN.md, "The digest chain") -- runs
// emit to produce the entries, and returns both digests.
func writeLayerStream(w io.Writer, emit func(*tar.Writer) error) (LayerResult, error) {
	counter := &countingWriter{w: w}
	gzipHash := sha256.New()

	// gzip.BestCompression with a pinned toolchain: Go's compress/gzip is
	// deterministic for a given Go version and level, so the Go version is
	// digest-affecting and is pinned by the flake.
	gzipWriter, err := gzip.NewWriterLevel(io.MultiWriter(counter, gzipHash), gzip.BestCompression)
	if err != nil {
		return LayerResult{}, fmt.Errorf("create gzip writer: %w", err)
	}

	tarHash := sha256.New()
	tarWriter := tar.NewWriter(io.MultiWriter(tarHash, gzipWriter))

	if err := emit(tarWriter); err != nil {
		return LayerResult{}, err
	}

	// Both closes matter: the tar trailer and the gzip footer are part of the
	// hashed bytes.
	if err := tarWriter.Close(); err != nil {
		return LayerResult{}, fmt.Errorf("close tar: %w", err)
	}
	if err := gzipWriter.Close(); err != nil {
		return LayerResult{}, fmt.Errorf("close gzip: %w", err)
	}

	return LayerResult{
		DiffID: digestOf(tarHash),
		Digest: digestOf(gzipHash),
		Size:   counter.n,
	}, nil
}

// writeEntries emits store paths, sorted by name, at their absolute locations.
// The store's parent dirs (/nix, /nix/store) have their mode forced (ancestors),
// because the host's mode for them is nondeterministic.
func writeEntries(tarWriter *tar.Writer, roots []string) error {
	paths, err := collectPaths(roots)
	if err != nil {
		return err
	}

	ancestors := make(map[string]bool)
	for _, d := range ancestorsOf(roots) {
		ancestors[d] = true
	}

	nameFn := func(p string, isDir bool) string { return tarName(p, isDir) }

	return writeEntryList(tarWriter, paths, nameFn, ancestors, nil)
}

// writeRootedEntries emits srcDir's contents, sorted, named relative to srcDir
// so they land at the image root. No ancestor-mode forcing: the caller controls
// modes (e.g. /tmp at 1777). own (may be nil) reassigns ownership per entry.
func writeRootedEntries(tarWriter *tar.Writer, srcDir string, own []OwnershipRule) error {
	paths, err := collectRootedPaths(srcDir)
	if err != nil {
		return err
	}

	nameFn := func(p string, isDir bool) string { return rootedName(srcDir, p, isDir) }

	return writeEntryList(tarWriter, paths, nameFn, nil, own)
}

// writeEntryList writes each path in order. ancestors and own may be nil.
func writeEntryList(tarWriter *tar.Writer, paths []string, nameFn func(string, bool) string, ancestors map[string]bool, own []OwnershipRule) error {
	// Records the first tar name emitted for each inode, so later paths sharing
	// it become hardlink entries rather than duplicate bodies.
	links := make(map[fileKey]string)

	for _, p := range paths {
		if err := writeEntry(tarWriter, p, nameFn, ancestors, links, own); err != nil {
			return fmt.Errorf("tar entry %s: %w", p, err)
		}
	}

	return nil
}

// fileKey identifies a file by inode, so two paths pointing at the same
// underlying inode (hardlinks) compare equal.
type fileKey struct {
	dev uint64
	ino uint64
}

// writeEntry writes a single path: header, then content for regular files.
// nameFn maps the on-disk path to its tar name; ancestors (may be nil) is the
// set of paths whose directory mode is forced; own (may be nil) reassigns the
// entry's uid/gid.
func writeEntry(tarWriter *tar.Writer, p string, nameFn func(string, bool) string, ancestors map[string]bool, links map[fileKey]string, own []OwnershipRule) error {
	info, err := os.Lstat(p)
	if err != nil {
		return fmt.Errorf("lstat: %w", err)
	}

	link := ""
	if info.Mode()&fs.ModeSymlink != 0 {
		if link, err = os.Readlink(p); err != nil {
			return fmt.Errorf("readlink: %w", err)
		}
	}

	header, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return fmt.Errorf("build header: %w", err)
	}

	normalizeHeader(header, nameFn(p, info.IsDir()), info, ancestors[p])

	// Ownership overrides the forced-root default from normalizeHeader. It runs
	// before linkIfSeen so a hardlink entry carries the same uid/gid.
	applyOwnership(header, own)

	if err := addXattrs(header, p, info); err != nil {
		return err
	}

	if handled, err := linkIfSeen(tarWriter, header, info, links); err != nil || handled {
		return err
	}

	if err := tarWriter.WriteHeader(header); err != nil {
		return fmt.Errorf("write header: %w", err)
	}

	if !info.Mode().IsRegular() {
		return nil
	}

	return copyFile(tarWriter, p)
}

// linkIfSeen preserves hardlinks. Nix's store optimiser points identical files
// at a single inode, frequently across store paths; without this each is emitted
// as a full copy, inflating the layer. The first path to reach an inode carries
// its bytes (and is recorded here); every later one is emitted as a bodyless
// TypeLink pointing at that first name. collectPaths sorts globally, so "first"
// is the lexicographically smallest name in the set -- stable across runs and
// independent of root order, which keeps the layer reproducible.
//
// It reports handled=true when it wrote a link entry; a false return means the
// caller should emit the entry normally.
func linkIfSeen(tarWriter *tar.Writer, header *tar.Header, info fs.FileInfo, links map[fileKey]string) (bool, error) {
	if !info.Mode().IsRegular() {
		return false, nil
	}

	key, nlink, ok := statKey(info)
	if !ok || nlink < 2 {
		return false, nil
	}

	target, seen := links[key]
	if !seen {
		links[key] = header.Name

		return false, nil
	}

	header.Typeflag = tar.TypeLink
	header.Linkname = target
	header.Size = 0

	if err := tarWriter.WriteHeader(header); err != nil {
		return false, fmt.Errorf("write hardlink header: %w", err)
	}

	return true, nil
}

// normalizeHeader strips every source of nondeterminism from a tar header:
// timestamps, ownership, and the host's mode bits on the store's parent dirs.
// forceMode replaces the entry's mode with ancestorDirMode (used for the store's
// parent directories, whose host mode is nondeterministic).
func normalizeHeader(header *tar.Header, name string, info fs.FileInfo, forceMode bool) {
	header.Name = name

	header.Uid, header.Gid = 0, 0
	header.Uname, header.Gname = "", ""

	header.ModTime = epochTime
	header.AccessTime = time.Time{}
	header.ChangeTime = time.Time{}

	// tar.FileInfoHeader cannot read a device's major/minor from an fs.FileInfo,
	// so fill them from the underlying stat for char/block devices; everything
	// else must carry 0/0 (the raw stat would otherwise leak the containing
	// filesystem's device numbers and defeat reproducibility).
	if header.Typeflag == tar.TypeChar || header.Typeflag == tar.TypeBlock {
		header.Devmajor, header.Devminor = deviceNumbers(info)
	} else {
		header.Devmajor, header.Devminor = 0, 0
	}

	// Store paths are long, so PAX is what we get anyway; pin it so the choice
	// never depends on path length. Go writes PAX records in sorted key order.
	header.Format = tar.FormatPAX

	if forceMode {
		header.Mode = ancestorDirMode
	}
}

// xattrPAXPrefix is the de-facto standard PAX key namespace for extended
// attributes, understood by GNU tar, bsdtar, umoci and containerd.
const xattrPAXPrefix = "SCHILY.xattr."

// addXattrs copies the file's allowlisted extended attributes into the header as
// PAX records. The headline case is security.capability (Linux file
// capabilities), which several image writers silently drop.
//
// Symlinks are skipped: their xattrs are read through to the target, which is
// not what we want, and store symlinks never carry meaningful ones.
func addXattrs(header *tar.Header, p string, info fs.FileInfo) error {
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil
	}

	attrs, err := readXattrs(p)
	if err != nil {
		return fmt.Errorf("read xattrs: %w", err)
	}

	for name, value := range attrs {
		if !xattrAllowed(name) {
			continue
		}

		if header.PAXRecords == nil {
			header.PAXRecords = make(map[string]string)
		}

		header.PAXRecords[xattrPAXPrefix+name] = value
	}

	return nil
}

// xattrAllowed is the reproducibility gate on which xattrs we carry. We take
// file capabilities and the user.* namespace, and deliberately drop everything
// else -- notably security.selinux (a host-dependent label) and system.* ACLs,
// which would make the layer digest vary by build host.
func xattrAllowed(name string) bool {
	return name == "security.capability" || strings.HasPrefix(name, "user.")
}

// copyFile streams a regular file's contents into the tar.
func copyFile(tarWriter *tar.Writer, p string) error {
	f, err := os.Open(p)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	// Read-only: a failed Close carries no information we can act on.
	defer func() { _ = f.Close() }()

	if _, err := io.Copy(tarWriter, f); err != nil {
		return fmt.Errorf("copy contents: %w", err)
	}

	return nil
}

// collectPaths returns every path to be archived -- the store's parent dirs
// plus a recursive walk of each root -- deduplicated and sorted by name.
//
// Sorting globally (rather than per-root) is what makes the layer a pure
// function of the set of roots, independent of the order they were passed in.
func collectPaths(roots []string) ([]string, error) {
	seen := make(map[string]bool)
	var paths []string

	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}

	ancestors := ancestorsOf(roots)
	for _, d := range ancestors {
		add(d)
	}

	for _, root := range roots {
		err := filepath.WalkDir(root, func(p string, _ fs.DirEntry, err error) error {
			if err != nil {
				return err //nolint:wrapcheck // propagating the walk's own error
			}
			add(p)

			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walk %s: %w", root, err)
		}
	}

	sort.Strings(paths)

	return paths, nil
}

// tarName converts an absolute path to its in-tar name: relative, with a
// trailing slash for directories.
func tarName(p string, isDir bool) string {
	name := strings.TrimPrefix(path.Clean(filepath.ToSlash(p)), "/")
	if isDir {
		name += "/"
	}

	return name
}

// collectRootedPaths returns every path under srcDir (excluding srcDir itself),
// sorted by name. Sorting globally makes the layer a pure function of the tree.
func collectRootedPaths(srcDir string) ([]string, error) {
	var paths []string

	err := filepath.WalkDir(srcDir, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err //nolint:wrapcheck // propagating the walk's own error
		}

		if p != srcDir {
			paths = append(paths, p)
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", srcDir, err)
	}

	sort.Strings(paths)

	return paths, nil
}

// rootedName is p's tar name relative to srcDir, so srcDir/etc/passwd becomes
// etc/passwd -- i.e. the content lands at the image root.
func rootedName(srcDir, p string, isDir bool) string {
	rel, err := filepath.Rel(srcDir, p)
	if err != nil {
		rel = strings.TrimPrefix(p, srcDir)
	}

	name := strings.TrimPrefix(filepath.ToSlash(rel), "/")
	if isDir {
		name += "/"
	}

	return name
}

func digestOf(h hash.Hash) string {
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// countingWriter records how many bytes reached the underlying writer, which is
// the compressed blob's size for the layer descriptor.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)

	if err != nil {
		return n, fmt.Errorf("write: %w", err)
	}

	return n, nil
}
