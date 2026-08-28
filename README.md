# nix-oci

A native, deterministic [OCI image layout](https://github.com/opencontainers/image-spec/blob/main/image-layout.md) writer for Nix.

[![CI](https://github.com/systemstart/nix-oci/actions/workflows/ci.yml/badge.svg)](https://github.com/systemstart/nix-oci/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/systemstart/nix-oci/branch/main/graph/badge.svg)](https://codecov.io/gh/systemstart/nix-oci)
[![License: GPL v3](https://img.shields.io/badge/License-GPLv3-blue.svg)](./LICENSE)

Give it a Nix closure, get a spec-compliant OCI image any tool can consume —
deterministic, layered for registry dedup, and daemon-free. Unlike `dockerTools`
(legacy `docker save` format) or `nix2container` (a non-standard description
needing its own transport), the output is a **plain OCI artifact** with nothing
else in the loop. See [DESIGN.md](./DESIGN.md) for the rationale and internals.

## What you get

- **Bit-for-bit reproducible** — identical inputs yield identical digests at
  every level (layer, config, manifest, index), so registries deduplicate
  unchanged blobs across rebuilds.
- **Layered for dedup** — one store path per layer, ranked by popularity, plus a
  customization layer for `/etc`, `/tmp`, and injected files.
- **Standard and self-contained** — a real OCI layout or oci-archive that
  skopeo, crane, podman, containerd, umoci, and any registry read directly. No
  custom transport, no daemon, no root.
- **Multi-arch** — an image index over per-platform manifests.

## How it works

The boundary is deliberately *closure-in, layout-out*. Computing the closure is
Nix's job (`nix path-info -r`, `closureInfo`); packaging it is ours. The writer
reads one store path per line on stdin and writes a layout directory:

```
image/
├── oci-layout            # {"imageLayoutVersion": "1.0.0"}
├── index.json            # entry point: manifest descriptors
└── blobs/sha256/
    ├── <digest>          # image manifest
    ├── <digest>          # image config
    └── <digest>          # layer blob (tar+gzip)
```

## Install

With flakes:

```sh
nix build github:systemstart/nix-oci      # result/bin/nix-oci
# or run without installing:
nix run github:systemstart/nix-oci -- version
```

From a clone, `nix build .#nix-oci`, or `go build ./cmd/nix-oci` if you already
have Go 1.26.5.

## Usage

### As a Nix derivation (recommended)

The flake exposes a `buildOCIImage` function that computes the closure with
`closureInfo` and runs the writer inside a derivation — no manual piping, and
the result is a reproducible store path:

```nix
# In a flake, with nix-oci as an input:
nix-oci.legacyPackages.${system}.buildOCIImage {
  name = "hello-oci";
  contents = [ pkgs.hello ];
  entrypoint = [ "${pkgs.hello}/bin/hello" ];
  # A customization layer for non-store content (root-owned, on top):
  extraCommands = ''
    mkdir -p etc tmp && chmod 1777 tmp
    echo 'root:x:0:0:root:/root:/bin/sh' > etc/passwd
  '';
  # cmd, env, arch, os, ref, maxLayers are also optional
}
```

`nix build` that attribute and you get the OCI layout as the output. Worked
instances live under this repo's flake `checks` (build one with
`nix build .#checks.x86_64-linux.exampleImage`). Pass `format = "archive"` to get
a single streamed oci-archive tar instead of a directory.

### On top of a base image (`fromImage`)

Layer on an existing base — its layers sit beneath yours and its config
(entrypoint, env, exposed ports) is inherited. `contents` may be empty when the
base and a customization layer supply everything:

```nix
buildOCIImage {
  name = "app-on-base";
  fromImage = baseLayout;   # a base OCI image layout
  # inherits the base's entrypoint/config; adds files at the image root:
  extraCommands = "mkdir -p srv && cp -r ${site} srv/www";
}
```

The output is a flat, standard OCI image (Docker layer media types in the base
are normalized to OCI). See `.#checks.x86_64-linux.exampleFromImage`.

### Cached, explicitly-layered builds

`buildOCIImageCached` gives each layer its own derivation, so an unchanged layer
is reused instead of recompressed. Name a stable base and let your app ride on
top — change the app and only the top layer rebuilds; the base is substituted
(and shared across every image that uses it):

```nix
buildOCIImageCached {
  name = "myapp";
  contents = [ myapp ];
  layers = [ [ pkgs.glibc ] ];   # bottom-to-top; each is a cached layer
}
```

The output is a standard OCI layout (byte-identical to `buildOCIImage` for the
same partition), it needs no experimental Nix features, and the tradeoff is that
you choose the layer boundaries rather than getting popularity ranking.

For a multi-arch image, build one layout per platform (each over a `pkgsCross`
closure) and tie them together with `buildOCIMultiArch`:

```nix
buildOCIMultiArch {
  name = "hello-multiarch";
  images = [
    (buildOCIImage {
      name = "hello-amd64"; arch = "amd64";
      contents = [ pkgs.pkgsCross.gnu64.hello ];
      entrypoint = [ "${pkgs.pkgsCross.gnu64.hello}/bin/hello" ];
    })
    (buildOCIImage {
      name = "hello-arm64"; arch = "arm64";
      contents = [ pkgs.pkgsCross.aarch64-multiplatform.hello ];
      entrypoint = [ "${pkgs.pkgsCross.aarch64-multiplatform.hello}/bin/hello" ];
    })
  ];
}
```

### As a CLI

The writer itself is closure-in, layout-out — hand it store paths on stdin:

```sh
nix build nixpkgs#hello
nix path-info -r ./result \
  | nix-oci build \
      --output ./image \
      --entrypoint "$(readlink -f ./result)/bin/hello"
```

### `nix-oci build` flags

| Flag | Default | Meaning |
|------|---------|---------|
| `--output` | *(required)* | Directory to write the OCI layout into |
| `--entrypoint` | — | Comma-separated entrypoint |
| `--cmd` | — | Comma-separated cmd |
| `--env` | — | Comma-separated environment (`KEY=VALUE`) |
| `--working-dir` | — | Working directory for the entrypoint |
| `--user` | — | User (`UID[:GID]` or name) the container runs as |
| `--exposed-ports` | — | Comma-separated ports to expose (e.g. `8080/tcp`) |
| `--volumes` | — | Comma-separated volume mount points |
| `--label` | — | Config label `KEY=VALUE` (repeatable) |
| `--stop-signal` | — | Signal that stops the container (e.g. `SIGTERM`) |
| `--annotation` | — | Manifest annotation `KEY=VALUE`, e.g. `org.opencontainers.image.source=…` (repeatable) |
| `--arch` | `amd64` | Image architecture |
| `--os` | `linux` | Image OS |
| `--ref` | `latest` | `org.opencontainers.image.ref.name` on the manifest |
| `--max-layers` | `100` | One store path per layer up to this cap; overflow shares the last layer. `1` = single layer |
| `--custom-layer` | — | Directory whose contents become a final layer at the image root (e.g. `/etc`, `/tmp`); root-owned unless `--own` says otherwise |
| `--own` | — | Set ownership of a custom-layer path: `PATH:UID:GID` (append `:r` for recursive), repeatable |
| `--from-image` | — | Path to a base OCI layout; our layers and config sit on top (see below) |
| `--archive` | off | Stream an oci-archive (tar of the layout) to stdout instead of writing a directory |

`--from-image` layers on an existing base (the `fromImage` param in the Nix
functions): the base's layers sit beneath ours and its config is inherited, with
`--entrypoint`/`--cmd`/`--env`/working-dir overriding. The output is a flat,
standard OCI image — "base image" is a build-time convenience, not something the
spec records. Docker layer media types in the base are normalized to OCI, so the
result stays all-OCI. With a base (or a customization layer), the closure may be
empty.

The Nix functions add three conveniences on top of these flags, for images that
expect conventional root paths or a writable per-user directory:

- **`rootLinks`** — `{ "bin/app" = "${app}/bin/app"; }` creates root-level
  symlinks. nix-oci keeps `contents` at their `/nix/store/…` paths, so this is
  how you make `/bin/app` (or `/etc/…`) resolve. The target must be in the
  closure.
- **`binLinks`** — `[ pkg ]` symlinks every file in each package's `bin/` into
  `/bin` (the dockerTools "merge into `/`" behaviour), and adds the package to
  the closure.
- **`chown`** — `[ { path = "home/app"; uid = 1000; gid = 1000; recursive =
  true; } ]` gives customization-layer paths a non-root owner (the CLI `--own`).
  Store content is always root-owned.

`nix-oci version` prints the version.

## Coming from a Dockerfile

The mental shift: nix-oci packages a **Nix closure**, not a sequence of build
steps. `RUN`/`apt-get` become Nix packages in `contents`; the config
instructions map one-to-one onto `buildOCIImage`.

| Dockerfile | `buildOCIImage` |
|---|---|
| `FROM img` | `fromImage = img;` (an OCI layout) — or omit for from-scratch |
| `RUN apt install foo` | add `pkgs.foo` to `contents` |
| `COPY ./x /app/x` | `extraCommands = "mkdir -p app && cp -r ${./x} app/x";` |
| `COPY --chown=1000 …` | `extraCommands` to stage + `chown = [ { path = …; uid = 1000; gid = 1000; } ];` |
| `WORKDIR /app` | `workingDir = "/app";` |
| `ENV K=V` | `env = [ "K=V" ];` |
| `ENTRYPOINT ["/bin/app"]` | `entrypoint = [ "/bin/app" ]; binLinks = [ app ];` (so `/bin/app` exists) — or point at the store path directly |
| `CMD ["--flag"]` | `cmd = [ "--flag" ];` |
| `USER 1000` | `user = "1000";` |
| `EXPOSE 8080` | `exposedPorts = [ "8080/tcp" ];` |
| `VOLUME /data` | `volumes = [ "/data" ];` |
| `LABEL k=v` | `labels.k = "v";` |
| `STOPSIGNAL SIGTERM` | `stopSignal = "SIGTERM";` |
| `HEALTHCHECK` / `SHELL` / `ONBUILD` | not supported — no OCI equivalent |

## Coming from dockerTools

`buildImage`/`buildLayeredImage` → `buildOCIImage`. Most options carry over by
name; the headline difference is the output — a standard OCI layout you push with
`skopeo`/`crane` and no daemon, rather than a docker-archive you `docker load`.

| dockerTools | nix-oci |
|---|---|
| `buildLayeredImage` / `buildImage` | `buildOCIImage` |
| `fromImage = pullImage {…}` | `fromImage = <OCI layout>;` — convert a docker-archive first: `skopeo copy docker-archive:… oci:…` |
| `contents` / `copyToRoot` (merged into `/`) | `contents` — store paths stay at `/nix/store/…`; use `binLinks`/`rootLinks` to expose `/bin/…` |
| `config.Cmd`/`.Entrypoint`/`.Env`/`.WorkingDir`/`.User`/`.ExposedPorts`/`.Labels` | `cmd`/`entrypoint`/`env`/`workingDir`/`user`/`exposedPorts`/`labels` |
| `extraCommands` | `extraCommands` (files at the image root, root-owned by default) |
| `fakeRootCommands` (e.g. `chown -R 1000 /home/app`) | `chown = [ { path = "home/app"; uid = 1000; gid = 1000; recursive = true; } ];` — declared, not run under fakeroot |
| `maxLayers` | `maxLayers` |
| `created` (defaults to a real build clock) | `created` — opt-in, defaults to the 1970 epoch; see [below](#image-created-timestamp) |
| **output**: docker-archive (`docker load`) | **output**: OCI layout (`skopeo copy` / `crane push`, no daemon) |

## Consuming the layout

The layout is a standard OCI artifact — inspect it, push it, or unpack it with
any OCI tool, no daemon required:

```sh
skopeo inspect oci:./image                          # parse manifest + config
skopeo copy oci:./image docker://registry/hello:v1  # push to any registry
umoci unpack --image ./image:latest bundle          # unpack to a runtime bundle
```

What has been verified end-to-end against the `hello` closure:

| Consumer | Path | Result |
|----------|------|--------|
| skopeo / crane | `oci:./image` | ✅ parses layout, manifest, config |
| Registry push | `skopeo copy oci: docker://` | ✅ digest round-trips byte-identically |
| umoci | `umoci unpack` | ✅ all store paths present, mtimes at epoch |
| OCI runtime | `crun run` on the bundle | ✅ prints `Hello, world!` |
| containerd | `ctr images import` (oci-archive) | ✅ imports; stored digest matches |
| Classic Docker | `skopeo copy … docker-daemon:` | ✅ transcodes and runs |

Classic Docker on the overlay2 graph driver **cannot** `docker load` an OCI
archive directly (it wants the legacy `docker save` format) — pipe through
skopeo as above. Docker with the containerd image store accepts OCI archives
natively. See the [consumption matrix](./DESIGN.md#consumption) for the full
detail, including the environment-blocked rows.

### Image `created` timestamp

By default the image config's `created` is the 1970 epoch, so the config blob is
a pure function of the content. Registry UIs render that as "56 years ago" with
no vendor, which is cosmetically poor but honest.

Set `created` if you want a real date. It takes RFC3339 or epoch seconds — the
latter because a flake's `self.lastModified` already is epoch seconds:

```nix
buildOCIImage {
  name = "app";
  contents = [ app ];
  created = self.lastModified;          # or "2026-08-27T09:56:00Z"
  labels = {
    "org.opencontainers.image.vendor" = "example";
    "org.opencontainers.image.revision" = self.rev or self.dirtyRev or "dev";
  };
}
```

Two things to know before you set it:

- It moves **only the config blob**. Tar entry mtimes stay at the epoch, so
  layer blobs keep their digests and stay shareable in a registry between
  images built with different timestamps.
- It costs cross-commit digest equality. Today two commits with identical
  content produce the same image, so a registry dedups them and a re-push can
  be skipped. With `created = self.lastModified`, every commit yields a new
  digest whether or not anything in the image changed. That is why the default
  stays at the epoch.

Pass something derived from the source — a commit timestamp — never a build
clock, or the build stops being reproducible. Note `self.lastModified` on a
dirty tree is the working-tree mtime, so local builds will churn.

## Reproducibility

Identical inputs produce byte-identical output — the same digest at every level.
`make repro` verifies it on one machine and CI compares digests across two, and
the property is enforced by unit tests (including feeding the closure in reversed
order). The determinism levers — epoch mtimes, forced ownership and parent-dir
modes, a pinned Go/gzip toolchain — are detailed in
[DESIGN.md](./DESIGN.md#determinism).

## Development

Everything runs through the pinned Nix dev shell:

```sh
nix develop            # pinned Go 1.26 + golangci-lint + goreleaser + git-cliff + oci tooling
make build             # go build ./...
make test              # tests + coverage, fails under 80%
make lint              # golangci-lint run
make fmt               # apply gofumpt
make cover             # open the HTML coverage report
```

The code map and design rationale live in [DESIGN.md](./DESIGN.md).

### Commit messages

Conventional commits — the version and the release notes are both derived from
them. Mark a breaking change with a `!` after the type **and** a
`BREAKING CHANGE:` footer in its own paragraph, separated by a blank line from
any trailers:

```
feat!: drop the v1 layout writer

Body explaining the change.

BREAKING CHANGE: `buildOCIImage` no longer accepts `legacyLayout`; remove it.

Co-Authored-By: Someone <someone@example.com>
```

Three tools have to agree on that shape, and only this one satisfies all three:

- **gsemver** picks up `BREAKING CHANGE:` but *not* the hyphenated
  `BREAKING-CHANGE:`, so the hyphenated form silently fails to bump the version.
- **git trailers** cannot parse a token containing a space, so putting
  `BREAKING CHANGE:` adjacent to `Co-Authored-By:` invalidates the whole trailer
  block. The blank line keeps the trailers parseable.
- **git-cliff** accepts either spelling and renders the footer text under the
  commit in the release notes.

The subject still has to stand on its own — say what broke in it, not only in
the footer.

### Releasing

Releases are cut by [GoReleaser](https://goreleaser.com) on a `v*` tag; CI
builds static binaries for linux/darwin × amd64/arm64 and attests provenance.
The release notes are generated by [git-cliff](https://git-cliff.org) from
`cliff.toml`, which owns the whole release body.

```sh
make release-tag-preview  # what version would be cut, without touching anything
make release-notes        # what the release page would say, without releasing
make release-tag          # gsemver computes the version, tags, pushes
make release              # git-cliff + goreleaser (CI runs this on the tag)
```

## License

[GPL-3.0](./LICENSE).
