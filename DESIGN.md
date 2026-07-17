# nix-oci — Design

This document is the rationale and internals: why nix-oci is built the way it
is, and the traps that shaped it. For how to *use* it, see the
[README](./README.md).

## Why a new writer

nixpkgs' `dockerTools` still emits the legacy `docker save` tarball — a
daemon-oriented format that isn't content-addressed as an artifact and is
awkward for anything that isn't `docker load`. The OCI Image Layout, by
contrast, is the container ecosystem's lingua franca (skopeo, crane, podman,
containerd, and every registry speak it) and is a natural fit for Nix because,
like the store, it is content-addressed all the way down.

nix-oci's position is **spec-first**: it emits a self-contained, standard OCI
artifact that any tool consumes with no special software. This is the deliberate
difference from the two neighbours — `dockerTools` produces a non-OCI legacy
format, and `nix2container` produces a non-standard JSON description whose
canonical consumer is its own skopeo transport. nix-oci hands you the actual
layout; nothing else needs to be in the loop.

## What it produces (and doesn't)

It writes a spec-compliant OCI Image Layout (a directory with `oci-layout`,
`index.json`, `blobs/sha256/`) and an oci-archive (a tar of that layout) for
single-file distribution. Output is fully reproducible — identical inputs yield
identical digests at every level — layered for cross-image registry dedup, and
multi-arch via the image index. It runs without root and without a daemon.

Out of scope, by design: compatibility with classic graph-driver `docker load`
(pipe through skopeo, or use Docker's containerd image store); image *execution*
or runtime-bundle generation (the OCI runtime spec's territory, covered by
umoci/crun); and building filesystem *content* — that stays ordinary Nix
derivation work. nix-oci packages closures, it doesn't create them.

## The code map

```
cmd/nix-oci/        CLI: build / combine / plan / build-layer / assemble
internal/oci/
  spec.go           OCI types + media types, re-exported from image-spec
  layer.go          deterministic tar + gzip, dual-hash single pass
  layout.go         layer/config/manifest/index emission; Plan
  graph.go          closure-graph parsing + popularity ranking
  layering.go       store-path -> layer assignment
  archive.go        oci-archive (tar of the layout)
  combine.go        multi-arch: merge layouts into one image index
  cached.go         BuildLayer / Assemble (incremental build path)
nix/
  build-image.nix         buildOCIImage (closureInfo -> writer)
  build-image-cached.nix  buildOCIImageCached (explicit cached layers)
  build-multiarch.nix     buildOCIMultiArch
  go-bin.nix              pinned Go toolchain (digest-affecting; see below)
```

The writer is **Go** because the conformance oracle (umoci) and primary
consumers are Go, `archive/tar` gives full header control, and
`opencontainers/image-spec` types are a free import. Its document types are
re-exported from `image-spec` (`specs-go/v1`) behind thin aliases in `spec.go`,
so the writer speaks one vocabulary with a single dependency point — and the
`history` ↔ `rootfs.diff_ids` alignment (a subtle spec trap) is defended by the
upstream struct definitions rather than hand-maintained.

## The digest chain

An OCI layout is content-addressed top to bottom, and that dictates build order:

1. Build each layer tar and hash it **uncompressed** → the `diffID` in the
   config's `rootfs.diff_ids`.
2. Compress the layer and hash the **compressed** blob → the digest in the
   manifest's layer descriptor.
3. Write the config (which embeds the diffIDs), hash it, reference it from the
   manifest.
4. Write the manifest, hash it, reference it from `index.json`.

Every layer is hashed **twice**, and no JSON document can be finalized until all
layers are. The layer path does both hashes in a **single pass** — a tee before
compression feeds the diffID hasher, a tee after it feeds the blob-digest hasher
— so content is never buffered or read twice. That property is what makes the
streaming archive possible without a second pass.

## Determinism

Because every artifact is digest-addressed, *any* nondeterminism invalidates the
whole chain and defeats registry dedup. The writer pins every lever:

- **Tar:** epoch mtimes; `atime`/`ctime` cleared; uid/gid forced to 0 with empty
  uname/gname; PAX format pinned (store paths exceed USTAR's 100-byte limit, and
  Go writes PAX records in sorted key order); entries sorted globally by name, so
  a layer is a pure function of the *set* of roots, not their enumeration order.
- **Parent directory modes are forced, not inherited.** The real `/nix/store` is
  commonly mode 1775 and varies between machines; inheriting it would make the
  layer digest host-dependent, so `/nix` and `/nix/store` are emitted at 0755
  unconditionally. This is a genuine reproducibility trap and easy to miss.
- **Compression** is Go's `compress/gzip` at `BestCompression`. That makes the
  *Go version* digest-affecting, so `nix/go-bin.nix` pins the toolchain (Go
  1.26.5, from upstream prebuilt tarballs), and the binary is built
  `CGO_ENABLED=0`, statically linked.

Acceptance test: build the same image twice on different machines and `diff -r`
— digests must be identical. `make repro` checks this on one machine (Nix
`--rebuild`); CI does it across two fresh runners and compares manifest digests.

## Layer-content fidelity

An OCI layer *is* a tar changeset, and `layer.md` governs what a conformant one
carries. nix-oci handles the parts writers commonly drop:

- **Hardlinks.** Nix's store optimiser points identical files at one inode,
  often across store paths. The writer tracks inodes: the first path to reach an
  inode carries its bytes; every later one becomes a bodyless `TypeLink`. "First"
  is the lexicographically smallest name (global sort), so it stays reproducible.
- **Extended attributes**, as `SCHILY.xattr.*` PAX records — the headline case is
  `security.capability` (Linux file capabilities). The set is gated to
  `security.capability` and the `user.*` namespace: host-dependent labels
  (`security.selinux`) and ACLs are dropped so they can't leak into the digest.
  (Nix's NAR format strips xattrs from store paths, so this mainly serves the
  customization layer and arbitrary roots.)
- **Device nodes** get their major/minor filled from the underlying stat
  (`tar.FileInfoHeader` can't supply them).

Xattrs and device handling are Linux-only (`//go:build linux`) with no-op stubs
elsewhere; hardlink detection is `//go:build unix`.

## Config translation

The OCI config is *almost* Docker's, minus `Healthcheck`, `Shell`, and `OnBuild`
(Docker extensions with no OCI equivalent). Every media type in the output is OCI
(`...manifest.v1+json`, `...config.v1+json`, `...layer.v1.tar+gzip`) — no Docker
media types appear anywhere.

## Layering

Layering is the highest-leverage decision for registry dedup. The default
(`buildOCIImage`) assigns **one store path per layer** up to `--max-layers`
(default 100; overlayfs degrades past ~125), then lumps the overflow into a final
layer. A store path packaged into two images therefore produces a byte-identical
layer blob in both, and the registry stores it once — the dedup property, which
is property-tested.

Which paths keep dedicated layers when the closure overflows the cap is decided
by **popularity**: `closureInfo`'s `registration` file gives the reference graph,
and each path is ranked by transitive-dependent count (how many closure members
ultimately depend on it). The deep base libraries stay isolated; the
image-specific tail is what gets lumped. It's a pure function of the closure, so
digests stay reproducible, and it matches nixpkgs' `referencesByPopularity`.

The config's `history` runs parallel to `rootfs.diff_ids`, one non-empty entry
per layer — the alignment several tools have shipped broken images over. It's
property-tested for any layer count.

The **customization layer** carries non-store content (`/etc` scaffolding,
`/tmp`, injected files): `WriteRootedLayer` maps a directory's contents to the
image root, forces them root-owned, and emits them as the topmost layer so they
shadow the store layers. `buildOCIImage` exposes it as `extraCommands`.

There is a real reproducibility trap here: `extraCommands` must stage into a
**build-local** directory, not a separate derivation. Nix store canonicalisation
strips write/sticky/setuid bits when a tree becomes a store path, so building the
staging tree as its own derivation silently turns `chmod 1777 tmp` into `0555`.
Staging inside the main build and packing it before any output is registered
preserves the modes.

## Streaming (oci-archive)

An OCI layout is a directory, not naturally a single streamable artifact — and a
blob's tar entry needs its name (which *is* its digest) and size up front. So the
archive is assembled in a temp dir (layer blobs already stream to temp files,
never buffered whole) and then tarred a file at a time, blobs-first /
`index.json`-last so a streaming reader sees every blob before the index
referencing it. Memory stays constant in image size, and the artifact never
lands in the store as a directory.

## Multi-arch

The image index ties per-platform manifests together. `combine` unions several
single-platform layouts' blobs (content-addressed, so shared blobs coalesce) and
writes a **nested** index: the layout's `index.json` points at one image-index
blob that lists the per-platform manifests (with `platform` descriptors,
`ref.name` stripped). The nesting is load-bearing — listing the manifests
directly in `index.json` makes tools read them as *separate* images. Each
per-platform layout is an independent derivation (remote builders parallelize
them); combine is the cheap finale that re-hashes nothing.

## Incremental builds (layer caching)

`buildOCIImage` is one derivation, so any closure change recompresses every
layer. The writer therefore also exposes the build as three composable steps:
`plan` (partition the closure — cheap, no compression), `build-layer` (compress
**one** layer to a blob + `meta.json`), and `assemble` (config/manifest/index
from precomputed layers). The result is **byte-identical** to a monolithic build
(`TestAssembleMatchesWrite` pins this with a full `diff -r`). Compression within
`Write` is also parallelized across CPUs (order-preserving, so output is
unchanged).

A *fully automatic* cached Nix function is not possible purely: the partition
depends on the closure graph, known only after building `closureInfo`, so reading
it back to spawn per-layer derivations needs import-from-derivation — and the
plan's store-path strings, read via `readFile`, have no string context, which
pure flake evaluation refuses to let a derivation depend on (`builtins.storePath`
is disallowed in pure eval, and `--impure` doesn't lift the `readFile`
check either). This is the same wall that makes `dockerTools` layer in one
derivation and nix2container's cached mode require explicit layers. The only pure
escape is experimental `dynamic-derivations` + `ca-derivations`, whose
portability cost isn't worth it.

So caching ships as **explicit layers** (`buildOCIImageCached`): each entry in
`layers` becomes its own derivation holding that closure minus the layers below
it, and the top layer holds the rest of `contents`. Layer derivations are named
independently of the image, so a base built from the same deps is one derivation
shared across every image that uses it and substitutable from a binary cache.
Change the app and only the top layer rebuilds; the base is reused. The trade,
by design, is that you name the boundaries — there's no popularity ranking on
this path (it stays on the automatic, uncached `buildOCIImage`).

The nix2container-style **lazy skopeo transport** is deliberately *not* built:
it would make the canonical artifact a non-standard description consumable only
by our own tool, forfeiting the spec-first identity. Its cross-build caching is
covered by explicit layers; its "skip blobs the registry has" is already free
with stock `skopeo copy`.

## Consumption

| Consumer | Path | Status |
|---|---|---|
| skopeo / crane | `oci:./image` | verified |
| Registry push | `skopeo copy oci: docker://` | verified (digest round-trips) |
| umoci | `umoci unpack` | verified |
| OCI runtime | `crun run` on the bundle | verified (`Hello, world!`) |
| containerd | `ctr images import` (oci-archive) | verified (digest matches) |
| Classic Docker | `skopeo copy … docker-daemon:` | verified (runs) |
| podman | `podman load` (oci-archive) | not verified here¹ |
| containerd store Docker | `docker load` (oci-archive) | not verified here¹ |

¹ Environment-blocked on the development host, not image-blocked: the `podman`
there is a remote/machine client with no local backend, and its Docker daemon
uses the overlay2 graph driver rather than the containerd image store. Classic
graph-driver `docker load` correctly *rejects* an oci-archive by design (it wants
the legacy format) — hence the skopeo `docker-daemon:` workaround.

## Limitations & open questions

- **Empty layers** in `buildOCIImageCached` (a layer whose paths are all already
  below it) fail loudly rather than being skipped.
- **zstd** layers are smaller and faster to pull but don't guarantee stable
  output across zstd releases; if added, the compressor version must be pinned
  and treated as digest-affecting. gzip stays the default for compatibility.
- **Provenance annotations** (`org.opencontainers.image.*`): which to emit by
  default is open, mindful of the privacy of embedding local paths / flake refs.
- **Upstreaming**: whether this eventually lands as a nixpkgs `ociTools`
  contribution, stays a standalone flake, or both.

## Prior art

`nix2container` (streaming, skopeo transport, layer-reuse JSON); nixpkgs
`streamLayeredImage` (Python tar writer, popularity layering); Bazel `rules_oci`
(hermetic OCI assembly under the same constraints); `umoci` (reference
implementation of the layout spec, the conformance oracle); and the OCI
image-spec itself.
