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
	"strings"

	"github.com/systemstart/nix-oci/internal/oci"
)

var version = "0.1.0"

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

	output := flags.String("output", "", "directory to write the OCI layout into (required)")
	entrypoint := flags.String("entrypoint", "", "comma-separated entrypoint")
	cmd := flags.String("cmd", "", "comma-separated cmd")
	env := flags.String("env", "", "comma-separated environment (KEY=VALUE)")
	arch := flags.String("arch", "amd64", "image architecture")
	osName := flags.String("os", "linux", "image OS")
	ref := flags.String("ref", "latest", "value for org.opencontainers.image.ref.name")
	customLayer := flags.String("custom-layer", "", "directory whose contents become a final layer at the image root")

	if err := parseFlags(flags, args); err != nil {
		return err
	}

	if *output == "" {
		return fmt.Errorf("--output is required")
	}

	opts := oci.ImageOptions{
		CustomLayer:  *customLayer,
		Entrypoint:   splitList(*entrypoint),
		Cmd:          splitList(*cmd),
		Env:          splitList(*env),
		Architecture: *arch,
		OS:           *osName,
		RefName:      *ref,
	}

	if err := oci.Assemble(*output, flags.Args(), opts); err != nil {
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

	output := flags.String("output", "", "directory to write the OCI layout into (required unless --archive)")
	archive := flags.Bool("archive", false, "write an oci-archive (tar of the layout) to stdout instead of a directory")
	entrypoint := flags.String("entrypoint", "", "comma-separated entrypoint")
	cmd := flags.String("cmd", "", "comma-separated cmd")
	env := flags.String("env", "", "comma-separated environment (KEY=VALUE)")
	arch := flags.String("arch", "amd64", "image architecture")
	osName := flags.String("os", "linux", "image OS")
	ref := flags.String("ref", "latest", "value for org.opencontainers.image.ref.name")
	maxLayers := flags.Int("max-layers", 100, "maximum layers; one store path per layer up to this cap, then the rest share the last")
	customLayer := flags.String("custom-layer", "", "directory whose contents become a final layer at the image root (e.g. /etc, /tmp)")
	closureGraph := flags.String("closure-graph", "", "path to closureInfo's registration file; enables popularity-ranked layering")

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

	if len(roots) == 0 {
		return fmt.Errorf("empty closure on stdin (expected one store path per line)")
	}

	opts := oci.ImageOptions{
		Roots:        roots,
		MaxLayers:    *maxLayers,
		CustomLayer:  *customLayer,
		ClosureGraph: *closureGraph,
		Entrypoint:   splitList(*entrypoint),
		Cmd:          splitList(*cmd),
		Env:          splitList(*env),
		Architecture: *arch,
		OS:           *osName,
		RefName:      *ref,
	}

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
