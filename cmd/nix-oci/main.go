// Command nix-oci writes OCI image layouts from Nix closures.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/systemstart/nix-oci/internal/oci"
)

// version is the fallback for `go run`/`go build`. Release binaries get the real
// version from the git tag (goreleaser's -X main.version), and Nix builds get it
// from the flake's ldflags -- so this string is never hand-bumped per release.
var version = "dev"

func main() {
	err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return // a subcommand's -h already printed its usage
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "nix-oci: %v\n", err)
		os.Exit(1)
	}
}

// run dispatches a subcommand. I/O is passed in rather than reached for through
// os.Stdin/Stdout so the CLI is testable without mutating process globals.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		usage(stderr)

		return errUsage()
	}

	switch args[0] {
	case "help", "-h", "--help":
		usage(stdout)

		return nil
	case "version", "--version":
		_, _ = fmt.Fprintln(stdout, version)

		return nil
	case "build":
		return build(args[1:], stdin, stdout, stderr)
	case "combine":
		return combine(args[1:], stdout, stderr)
	case "plan":
		return planCmd(args[1:], stdin, stdout, stderr)
	case "build-layer":
		return buildLayerCmd(args[1:], stdin, stderr)
	case "assemble":
		return assembleCmd(args[1:], stderr)
	default:
		return fmt.Errorf("unknown command %q (run 'nix-oci help')", args[0])
	}
}

func errUsage() error {
	return fmt.Errorf("no command given (run 'nix-oci help')")
}

// usage prints the top-level command overview.
func usage(w io.Writer) {
	_, _ = fmt.Fprint(w, `nix-oci -- a deterministic OCI image writer for Nix

Usage: nix-oci <command> [flags]

Commands:
  build         Write an OCI layout (or --archive) from a closure on stdin
  combine       Merge single-platform layouts into a multi-arch image
  version       Print the version
  help          Show this help

Cached-build plumbing (usually driven by the Nix functions, not by hand):
  plan          Print the layer partition for a closure
  build-layer   Compress one layer into a cacheable blob
  assemble      Build a layout from precomputed layers

Run 'nix-oci <command> -h' for a command's flags.
`)
}

// parseFlags parses a subcommand's flags, mapping -h/--help (which the flag
// package prints usage for) to flag.ErrHelp so main can exit cleanly.
func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err //nolint:wrapcheck // sentinel checked by main to exit cleanly
		}

		return fmt.Errorf("parse flags: %w", err)
	}

	return nil
}

// kvFlag is a repeatable KEY=VALUE flag collected into a map.
type kvFlag map[string]string

func (f kvFlag) String() string { return "" }

func (f kvFlag) Set(s string) error {
	key, value, ok := strings.Cut(s, "=")
	if !ok {
		return fmt.Errorf("expected KEY=VALUE, got %q", s)
	}

	f[key] = value

	return nil
}

// ownFlag is a repeatable ownership flag, PATH:UID:GID[:r], collected in order.
// It sets uid/gid on customization-layer paths (the fakeRootCommands/chown
// stand-in). A trailing ":r" makes the rule cover everything under PATH.
type ownFlag []oci.OwnershipRule

func (f *ownFlag) String() string { return "" }

func (f *ownFlag) Set(s string) error {
	// Split from the right so a PATH containing ':' still parses: an optional
	// "r"/"R" recursion marker, then GID, then UID, then PATH is the remainder.
	parts := strings.Split(s, ":")

	recursive := false
	if last := parts[len(parts)-1]; last == "r" || last == "R" {
		recursive = true
		parts = parts[:len(parts)-1]
	}

	if len(parts) < 3 {
		return fmt.Errorf("expected PATH:UID:GID[:r], got %q", s)
	}

	uid, err := strconv.Atoi(parts[len(parts)-2])
	if err != nil {
		return fmt.Errorf("bad uid in %q: %w", s, err)
	}

	gid, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		return fmt.Errorf("bad gid in %q: %w", s, err)
	}

	path := strings.Join(parts[:len(parts)-2], ":")
	if path == "" {
		return fmt.Errorf("empty path in %q", s)
	}

	*f = append(*f, oci.OwnershipRule{Path: path, UID: uid, GID: gid, Recursive: recursive})

	return nil
}

// imageFlags are the config-shaping flags shared by build and assemble.
type imageFlags struct {
	entrypoint   *string
	cmd          *string
	env          *string
	workingDir   *string
	arch         *string
	osName       *string
	ref          *string
	user         *string
	exposedPorts *string
	volumes      *string
	stopSignal   *string
	customLayer  *string
	labels       kvFlag
	annotations  kvFlag
	own          ownFlag
}

func registerImageFlags(fs *flag.FlagSet) *imageFlags {
	f := &imageFlags{
		entrypoint:   fs.String("entrypoint", "", "comma-separated entrypoint"),
		cmd:          fs.String("cmd", "", "comma-separated cmd"),
		env:          fs.String("env", "", "comma-separated environment (KEY=VALUE)"),
		workingDir:   fs.String("working-dir", "", "working directory for the entrypoint"),
		arch:         fs.String("arch", "amd64", "image architecture"),
		osName:       fs.String("os", "linux", "image OS"),
		ref:          fs.String("ref", "latest", "value for org.opencontainers.image.ref.name"),
		user:         fs.String("user", "", "user (UID[:GID] or name) the container runs as"),
		exposedPorts: fs.String("exposed-ports", "", "comma-separated ports to expose (e.g. 8080/tcp)"),
		volumes:      fs.String("volumes", "", "comma-separated volume mount points"),
		stopSignal:   fs.String("stop-signal", "", "signal that stops the container (e.g. SIGTERM)"),
		customLayer:  fs.String("custom-layer", "", "directory whose contents become a final layer at the image root"),
		labels:       kvFlag{},
		annotations:  kvFlag{},
	}

	fs.Var(f.labels, "label", "config label KEY=VALUE (repeatable)")
	fs.Var(f.annotations, "annotation", "manifest annotation KEY=VALUE, e.g. org.opencontainers.image.source (repeatable)")
	fs.Var(&f.own, "own", "set ownership of a custom-layer path: PATH:UID:GID[:r] for recursive (repeatable)")

	return f
}

func (f *imageFlags) options() oci.ImageOptions {
	return oci.ImageOptions{
		Entrypoint:   splitList(*f.entrypoint),
		Cmd:          splitList(*f.cmd),
		Env:          splitList(*f.env),
		WorkingDir:   *f.workingDir,
		Architecture: *f.arch,
		OS:           *f.osName,
		RefName:      *f.ref,
		User:         *f.user,
		ExposedPorts: splitList(*f.exposedPorts),
		Volumes:      splitList(*f.volumes),
		StopSignal:   *f.stopSignal,
		CustomLayer:  *f.customLayer,
		Ownership:    f.own,
		Labels:       nilIfEmpty(f.labels),
		Annotations:  nilIfEmpty(f.annotations),
	}
}

func nilIfEmpty(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}

	return m
}

// planCmd prints the layer partition (JSON) for a closure on stdin, without
// compressing anything -- the cheap first step of the cached build path.
func planCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("plan", flag.ContinueOnError)
	flags.SetOutput(stderr)

	maxLayers := flags.Int("max-layers", 100, "maximum layers")
	closureGraph := flags.String("closure-graph", "", "path to closureInfo's registration file")

	if err := parseFlags(flags, args); err != nil {
		return err
	}

	roots, err := readClosure(stdin)
	if err != nil {
		return err
	}

	if len(roots) == 0 {
		return fmt.Errorf("empty closure on stdin (expected one store path per line)")
	}

	groups, err := oci.Plan(oci.ImageOptions{Roots: roots, MaxLayers: *maxLayers, ClosureGraph: *closureGraph})
	if err != nil {
		return fmt.Errorf("plan: %w", err)
	}

	if err := json.NewEncoder(stdout).Encode(map[string][][]string{"layers": groups}); err != nil {
		return fmt.Errorf("encode plan: %w", err)
	}

	return nil
}

// buildLayerCmd compresses one layer into --output as a cacheable blob +
// metadata. Its store paths come from the positional arguments, or from stdin
// (one per line) when none are given -- the latter suits a Nix build that
// computes the path set (closure minus lower layers) on the fly.
func buildLayerCmd(args []string, stdin io.Reader, stderr io.Writer) error {
	flags := flag.NewFlagSet("build-layer", flag.ContinueOnError)
	flags.SetOutput(stderr)

	output := flags.String("output", "", "directory to write the layer blob and metadata into (required)")

	if err := parseFlags(flags, args); err != nil {
		return err
	}

	if *output == "" {
		return fmt.Errorf("--output is required")
	}

	paths := flags.Args()
	if len(paths) == 0 {
		var err error
		if paths, err = readClosure(stdin); err != nil {
			return err
		}
	}

	if len(paths) == 0 {
		return fmt.Errorf("build-layer needs at least one store path (as arguments or on stdin)")
	}

	if err := oci.BuildLayer(*output, paths); err != nil {
		return fmt.Errorf("build layer: %w", err)
	}

	return nil
}

// assembleCmd builds a layout from precomputed layer directories (positional
// arguments, in order) without recompressing them.
func assembleCmd(args []string, stderr io.Writer) error {
	flags := flag.NewFlagSet("assemble", flag.ContinueOnError)
	flags.SetOutput(stderr)

	img := registerImageFlags(flags)
	output := flags.String("output", "", "directory to write the OCI layout into (required)")

	if err := parseFlags(flags, args); err != nil {
		return err
	}

	if *output == "" {
		return fmt.Errorf("--output is required")
	}

	if err := oci.Assemble(*output, flags.Args(), img.options()); err != nil {
		return fmt.Errorf("assemble: %w", err)
	}

	_, _ = fmt.Fprintf(stderr, "assembled %s from %d layers\n", *output, len(flags.Args()))

	return nil
}

// combine merges single-platform layouts (given as positional arguments) into
// one multi-arch layout or archive.
func combine(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("combine", flag.ContinueOnError)
	flags.SetOutput(stderr)

	output := flags.String("output", "", "directory to write the combined layout into (required unless --archive)")
	archive := flags.Bool("archive", false, "write an oci-archive to stdout instead of a directory")
	ref := flags.String("ref", "latest", "value for org.opencontainers.image.ref.name on the image index")

	if err := parseFlags(flags, args); err != nil {
		return err
	}

	inputs := flags.Args()
	if len(inputs) == 0 {
		return fmt.Errorf("combine needs at least one input layout")
	}

	if *archive {
		if err := oci.CombineArchive(stdout, inputs, *ref); err != nil {
			return fmt.Errorf("combine archive: %w", err)
		}

		_, _ = fmt.Fprintf(stderr, "combined %d layouts into an oci-archive on stdout\n", len(inputs))

		return nil
	}

	if *output == "" {
		return fmt.Errorf("--output is required (or use --archive to stream to stdout)")
	}

	if err := oci.Combine(*output, inputs, *ref); err != nil {
		return fmt.Errorf("combine: %w", err)
	}

	_, _ = fmt.Fprintf(stderr, "combined %d layouts into %s\n", len(inputs), *output)

	return nil
}

// build reads a closure (one store path per line, on stdin -- i.e. the output of
// `nix path-info -r`) and writes an OCI layout.
//
// Computing the closure is deliberately not our job: `nix path-info -r` and
// closureInfo already do it, and shelling out keeps the spike honest about
// where the boundary is.
func build(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("build", flag.ContinueOnError)
	flags.SetOutput(stderr)

	img := registerImageFlags(flags)
	output := flags.String("output", "", "directory to write the OCI layout into (required unless --archive)")
	archive := flags.Bool("archive", false, "write an oci-archive (tar of the layout) to stdout instead of a directory")
	maxLayers := flags.Int("max-layers", 100, "maximum layers; one store path per layer up to this cap, then the rest share the last")
	closureGraph := flags.String("closure-graph", "", "path to closureInfo's registration file; enables popularity-ranked layering")
	fromImage := flags.String("from-image", "", "path to a base OCI image layout; our layers and config sit on top of it")

	if err := parseFlags(flags, args); err != nil {
		return err
	}

	if *output == "" && !*archive {
		return fmt.Errorf("--output is required (or use --archive to stream to stdout)")
	}

	roots, err := readClosure(stdin)
	if err != nil {
		return err
	}

	if err := requireContent(roots, *fromImage, *img.customLayer); err != nil {
		return err
	}

	opts := img.options()
	opts.Roots = roots
	opts.MaxLayers = *maxLayers
	opts.ClosureGraph = *closureGraph
	opts.BaseImage = *fromImage

	if *archive {
		if err := oci.WriteArchive(stdout, opts); err != nil {
			return fmt.Errorf("write archive: %w", err)
		}

		_, _ = fmt.Fprintf(stderr, "wrote oci-archive to stdout (%d store paths)\n", len(roots))

		return nil
	}

	if err := oci.Write(*output, opts); err != nil {
		return fmt.Errorf("write layout: %w", err)
	}

	_, _ = fmt.Fprintf(stderr, "wrote %s (%d store paths)\n", *output, len(roots))

	return nil
}

// requireContent rejects a build with nothing to package: a base image or a
// customization layer can each supply content, so an empty closure is only an
// error when there is nothing else.
func requireContent(roots []string, fromImage, customLayer string) error {
	if len(roots) == 0 && fromImage == "" && customLayer == "" {
		return fmt.Errorf("empty closure on stdin (expected one store path per line, or pass --from-image/--custom-layer)")
	}

	return nil
}

// readClosure parses one store path per line, ignoring blanks.
func readClosure(r io.Reader) ([]string, error) {
	var roots []string

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" {
			roots = append(roots, line)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read closure: %w", err)
	}

	return roots, nil
}

func splitList(s string) []string {
	if s == "" {
		return nil
	}

	return strings.Split(s, ",")
}
