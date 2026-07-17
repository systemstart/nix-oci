package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/systemstart/nix-oci/internal/oci"
)

// fakeStore builds a minimal store-path-shaped tree and returns its absolute
// path. The CLI treats roots as opaque paths, so this need not live under a real
// /nix/store.
func fakeStore(t *testing.T) string {
	t.Helper()

	root := filepath.Join(t.TempDir(), "store", "aaa-prog")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(root, "bin", "prog"), []byte("#!/bin/sh\necho hi\n"), 0o555); err != nil {
		t.Fatalf("write: %v", err)
	}

	return root
}

// runCLI invokes run() with the given args and stdin, capturing stdout/stderr.
func runCLI(t *testing.T, stdin string, args ...string) (stdout, stderr string, err error) {
	t.Helper()

	var out, errBuf bytes.Buffer
	err = run(args, strings.NewReader(stdin), &out, &errBuf)

	return out.String(), errBuf.String(), err
}

// TestBuildEmitsValidLayout is the CLI-level happy path: feed a closure on
// stdin, then assert the emitted directory is a layout whose digest chain
// resolves end to end and whose config carries the flags we passed.
func TestBuildEmitsValidLayout(t *testing.T) {
	t.Parallel()

	root := fakeStore(t)
	out := filepath.Join(t.TempDir(), "image")

	_, stderr, err := runCLI(t, root+"\n",
		"build",
		"--output", out,
		"--entrypoint", root+"/bin/prog",
		"--env", "PATH=/bin,LANG=C",
		"--ref", "hello:latest",
	)
	if err != nil {
		t.Fatalf("build: %v (stderr: %s)", err, stderr)
	}

	if !strings.Contains(stderr, "wrote "+out) {
		t.Errorf("expected a progress line on stderr, got: %q", stderr)
	}

	marker := readJSON[map[string]string](t, filepath.Join(out, "oci-layout"))
	if marker["imageLayoutVersion"] != "1.0.0" {
		t.Errorf("oci-layout version = %q, want 1.0.0", marker["imageLayoutVersion"])
	}

	md := manifestDescriptor(t, out, "hello:latest")
	manifest := readBlob[oci.Manifest](t, out, string(md.Digest))
	config := readBlob[oci.Config](t, out, string(manifest.Config.Digest))

	assertConfigContents(t, config, manifest, root+"/bin/prog")
}

// assertConfigContents checks the image config records the entrypoint and env we
// passed, and that its rootfs holds exactly one diffID distinct from the
// compressed layer digest.
func assertConfigContents(t *testing.T, config oci.Config, manifest oci.Manifest, wantEntrypoint string) {
	t.Helper()

	if got := config.Config.Entrypoint; len(got) != 1 || got[0] != wantEntrypoint {
		t.Errorf("entrypoint = %v, want [%s]", got, wantEntrypoint)
	}

	if got := config.Config.Env; len(got) != 2 || got[0] != "PATH=/bin" || got[1] != "LANG=C" {
		t.Errorf("env = %v, want [PATH=/bin LANG=C]", got)
	}

	if len(config.RootFS.DiffIDs) != 1 || config.RootFS.DiffIDs[0] == manifest.Layers[0].Digest {
		t.Errorf("diff_ids = %v; must be one entry, distinct from the compressed layer digest", config.RootFS.DiffIDs)
	}
}

// manifestDescriptor reads index.json, asserts it names exactly one OCI manifest
// with the expected ref tag, and returns that descriptor.
func manifestDescriptor(t *testing.T, layoutDir, wantRef string) oci.Descriptor {
	t.Helper()

	index := readJSON[oci.Index](t, filepath.Join(layoutDir, "index.json"))
	if len(index.Manifests) != 1 {
		t.Fatalf("index has %d manifests, want 1", len(index.Manifests))
	}

	md := index.Manifests[0]
	if md.MediaType != oci.MediaTypeManifest {
		t.Errorf("manifest mediaType = %q", md.MediaType)
	}

	if md.Annotations[oci.AnnotationRefName] != wantRef {
		t.Errorf("ref annotation = %q, want %q", md.Annotations[oci.AnnotationRefName], wantRef)
	}

	return md
}

// TestBuildNoDockerMediaTypes guards the "no Docker media types anywhere" rule
// by scanning every emitted file's raw bytes for the docker vendor prefix.
// TestBuildConfigFlags covers the shared config flags: repeatable --label /
// --annotation and the runtime-config passthrough land in the output.
func TestBuildConfigFlags(t *testing.T) {
	t.Parallel()

	root := fakeStore(t)
	out := filepath.Join(t.TempDir(), "img")

	if _, stderr, err := runCLI(t, root, "build", "--output", out,
		"--user", "1000",
		"--exposed-ports", "8080/tcp",
		"--label", "title=demo",
		"--annotation", "org.opencontainers.image.source=https://ex",
	); err != nil {
		t.Fatalf("build: %v (%s)", err, stderr)
	}

	md := manifestDescriptor(t, out, "latest")
	manifest := readBlob[oci.Manifest](t, out, string(md.Digest))

	if manifest.Annotations["org.opencontainers.image.source"] != "https://ex" {
		t.Errorf("source annotation = %v", manifest.Annotations)
	}

	config := readBlob[oci.Config](t, out, string(manifest.Config.Digest))

	if config.Config.User != "1000" {
		t.Errorf("user = %q", config.Config.User)
	}

	if config.Config.Labels["title"] != "demo" {
		t.Errorf("labels = %v", config.Config.Labels)
	}

	if _, ok := config.Config.ExposedPorts["8080/tcp"]; !ok {
		t.Errorf("exposed ports = %v", config.Config.ExposedPorts)
	}
}

func TestBuildNoDockerMediaTypes(t *testing.T) {
	t.Parallel()

	root := fakeStore(t)
	out := filepath.Join(t.TempDir(), "image")

	if _, stderr, err := runCLI(t, root, "build", "--output", out); err != nil {
		t.Fatalf("build: %v (%s)", err, stderr)
	}

	err := filepath.WalkDir(out, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		if data, readErr := os.ReadFile(p); readErr == nil && bytes.Contains(data, []byte("vnd.docker")) {
			t.Errorf("%s contains a Docker media type", p)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

// TestCombineCLI drives the combine subcommand end to end: build two
// single-platform layouts, merge them into a multi-arch layout (one image-index
// entry on top), and stream the archive variant.
func TestCombineCLI(t *testing.T) {
	t.Parallel()

	root := fakeStore(t)
	work := t.TempDir()
	amd := filepath.Join(work, "amd")
	arm := filepath.Join(work, "arm")

	for _, tc := range []struct{ out, arch string }{{amd, "amd64"}, {arm, "arm64"}} {
		if _, stderr, err := runCLI(t, root, "build", "--output", tc.out, "--arch", tc.arch); err != nil {
			t.Fatalf("build %s: %v (%s)", tc.arch, err, stderr)
		}
	}

	out := filepath.Join(work, "multi")
	if _, _, err := runCLI(t, "", "combine", "--output", out, amd, arm); err != nil {
		t.Fatalf("combine: %v", err)
	}

	top := readJSON[oci.Index](t, filepath.Join(out, "index.json"))
	if len(top.Manifests) != 1 || top.Manifests[0].MediaType != oci.MediaTypeIndex {
		t.Errorf("combined top index = %+v, want one image-index entry", top.Manifests)
	}

	stdout, _, err := runCLI(t, "", "combine", "--archive", amd, arm)
	if err != nil {
		t.Fatalf("combine --archive: %v", err)
	}

	if len(stdout) == 0 {
		t.Error("combine --archive produced no output")
	}
}

// TestCachedPathCLI drives the incremental build: plan -> build-layer (per
// group) -> assemble, and checks the result is a valid layout.
func TestCachedPathCLI(t *testing.T) {
	t.Parallel()

	root := fakeStore(t)
	work := t.TempDir()

	planJSON, _, err := runCLI(t, root, "plan")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	var plan struct {
		Layers [][]string `json:"layers"`
	}
	if err := json.Unmarshal([]byte(planJSON), &plan); err != nil {
		t.Fatalf("parse plan: %v", err)
	}

	if len(plan.Layers) == 0 {
		t.Fatal("plan produced no layers")
	}

	layerDirs := make([]string, len(plan.Layers))
	for i, group := range plan.Layers {
		dir := filepath.Join(work, "layer"+strconv.Itoa(i))
		args := append([]string{"build-layer", "--output", dir}, group...)

		if _, stderr, err := runCLI(t, "", args...); err != nil {
			t.Fatalf("build-layer %d: %v (%s)", i, err, stderr)
		}

		layerDirs[i] = dir
	}

	out := filepath.Join(work, "image")
	args := append([]string{"assemble", "--output", out, "--entrypoint", "/x"}, layerDirs...)

	if _, stderr, err := runCLI(t, "", args...); err != nil {
		t.Fatalf("assemble: %v (%s)", err, stderr)
	}

	index := readJSON[oci.Index](t, filepath.Join(out, "index.json"))
	if len(index.Manifests) != 1 {
		t.Errorf("assembled layout index has %d manifests, want 1", len(index.Manifests))
	}
}

// TestBuildLayerStdin covers build-layer reading its paths from stdin (the form
// the cached Nix function uses, piping closure-minus-lower-layers).
func TestBuildLayerStdin(t *testing.T) {
	t.Parallel()

	root := fakeStore(t)
	out := filepath.Join(t.TempDir(), "layer")

	if _, stderr, err := runCLI(t, root+"\n", "build-layer", "--output", out); err != nil {
		t.Fatalf("build-layer: %v (%s)", err, stderr)
	}

	if _, err := os.Stat(filepath.Join(out, "meta.json")); err != nil {
		t.Errorf("build-layer produced no meta.json: %v", err)
	}
}

func TestRunErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		stdin   string
		wantErr string
	}{
		{"no args", nil, "", "no command given"},
		{"unknown command", []string{"frobnicate"}, "", `unknown command "frobnicate"`},
		{"build without output", []string{"build"}, "/nix/store/x", "--output is required"},
		{"build empty closure", []string{"build", "--output", "unused"}, "\n  \n", "empty closure"},
		{"build bad flag", []string{"build", "--nope"}, "", "parse flags"},
		{"build bad label", []string{"build", "--output", "x", "--label", "noequals"}, "", "KEY=VALUE"},
		{"combine no inputs", []string{"combine", "--output", "x"}, "", "at least one input"},
		{"combine no output", []string{"combine", "some-layout"}, "", "--output is required"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, _, err := runCLI(t, tt.stdin, tt.args...)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}

			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestVersion(t *testing.T) {
	t.Parallel()

	for _, arg := range []string{"version", "--version"} {
		stdout, _, err := runCLI(t, "", arg)
		if err != nil {
			t.Fatalf("%s: %v", arg, err)
		}

		if strings.TrimSpace(stdout) != version {
			t.Errorf("%s printed %q, want %q", arg, strings.TrimSpace(stdout), version)
		}
	}
}

func TestHelp(t *testing.T) {
	t.Parallel()

	for _, arg := range []string{"help", "-h", "--help"} {
		stdout, _, err := runCLI(t, "", arg)
		if err != nil {
			t.Fatalf("%s: %v", arg, err)
		}

		if !strings.Contains(stdout, "Commands:") {
			t.Errorf("%s did not print the command overview: %q", arg, stdout)
		}
	}
}

// readJSON reads and unmarshals a JSON file into T.
func readJSON[T any](t *testing.T, path string) T {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}

	return v
}

// readBlob resolves a "sha256:<hex>" digest to its blob file and parses it as T.
func readBlob[T any](t *testing.T, layoutDir, digest string) T {
	t.Helper()

	hexDigest, ok := strings.CutPrefix(digest, "sha256:")
	if !ok {
		t.Fatalf("digest %q lacks sha256: prefix", digest)
	}

	return readJSON[T](t, filepath.Join(layoutDir, "blobs", "sha256", hexDigest))
}
