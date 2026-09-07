#!/usr/bin/env bash
# Answer "does moving nix-oci change the digests of the images I build?"
#
# The question sounds like a job for `diff -r` over two image layouts, and it
# is not: that comparison answers a broader question and reports differences
# this one is not asking about. Two artefacts make a digest-neutral bump look
# like a re-digesting one, and both have burned somebody:
#
#   1. `--override-input` makes the flake dirty, so `self.rev` becomes
#      unavailable. A repo that stamps it into image metadata (a
#      `org.opencontainers.image.revision` annotation, say) gets a different
#      value under the override than in a normal build. That moves the config
#      and manifest digests while every layer stays byte-identical.
#
#   2. Editing flake.nix or flake.lock to compare before/after changes the
#      flake source, so any binary built *from* that source lands on a new
#      store path -- and store paths appear inside the layer tar, as entry
#      names and as symlink targets. Layers then differ with no toolchain
#      change at all. This script never edits the pin, which is why it takes
#      the comparison revision as an argument.
#
# Layer digests are what actually answer the question: they are the bytes the
# compressor produces, and they are immune to both artefacts. So they are
# compared first and reported separately from the manifest.
#
# Usage:
#   scripts/diff-digests.sh -r REF [-a ATTR] [-i INPUT] [-- BUILD_ARGS...]
#
#   -r REF     flake reference to compare against, e.g.
#              github:systemstart/nix-oci/v0.6.0, or a local checkout path
#   -a ATTR    image attribute to build, without the leading '.#'
#              (default: packages.<current-system>.default)
#   -i INPUT   flake input to override (default: nix-oci)
#   -- ARGS    extra flags passed to *both* builds, e.g. `-- --impure`.
#              Whatever your normal build needs must appear here: omitting a
#              flag silently builds a different image and compares that.
#
# Run it from the repo whose images you are asking about, not from nix-oci.
#
# Informational: the exit status reports whether the comparison ran, not
# whether anything differed.
set -euo pipefail

die() {
    echo "diff-digests: $*" >&2
    exit 1
}

usage() {
    sed -n '/^# Usage:/,/^# Informational/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

attr=""
input="nix-oci"
ref=""

while getopts ":a:i:r:h" opt; do
    case "$opt" in
        a) attr="$OPTARG" ;;
        i) input="$OPTARG" ;;
        r) ref="$OPTARG" ;;
        h) usage; exit 0 ;;
        :) die "-$OPTARG needs an argument" ;;
        *) die "unknown flag -$OPTARG (try -h)" ;;
    esac
done
shift $((OPTIND - 1))
extra=("$@")

for tool in nix jq; do
    command -v "$tool" >/dev/null || die "$tool is not on PATH (run inside \`nix develop\`)"
done
[ -n "$ref" ] || die "-r REF is required (try -h)"
[ -f flake.nix ] || die "no flake.nix here — run this from the repo that builds the images"

# `--override-input` on a locked flake makes Nix announce, in several lines,
# that it is not writing the lock file -- restating the override this script
# has already printed. Filter that from a successful build; a failure prints
# its stderr untouched.
run_quiet() {
    local err rc=0 out
    err="$(mktemp)"
    out="$("$@" 2>"$err")" || rc=$?
    [ "$rc" -eq 0 ] || cat "$err" >&2
    rm -f "$err"
    [ "$rc" -eq 0 ] || return "$rc"
    printf '%s' "$out"
}

system="$(nix eval --raw --impure --expr builtins.currentSystem)"
attr="${attr:-packages.$system.default}"

# Emits "<digest> <size>" per layer, for every manifest in the index, so a
# multi-arch image is compared per platform rather than collapsed.
layers_of() {
    local out="$1" md
    while read -r md; do
        jq -r '.layers[] | "\(.digest) \(.size)"' "$out/blobs/sha256/${md#sha256:}"
    done < <(jq -r '.manifests[].digest' "$out/index.json")
}

# The manifest and config digests move for reasons that are not the
# compressor -- annotations, the config blob, a `created` timestamp -- so they
# are reported apart from the layers rather than mixed in.
manifests_of() {
    local out="$1" md
    while read -r md; do
        printf 'manifest %s\n' "$md"
        jq -r '"config   \(.config.digest)"' "$out/blobs/sha256/${md#sha256:}"
    done < <(jq -r '.manifests[].digest' "$out/index.json")
}

# The decoded manifest and config of every image in the index, key-sorted, so
# a diff of these names the field that moved instead of a digest that did.
contents_of() {
    local out="$1" md cd
    while read -r md; do
        jq -S 'del(.layers)' "$out/blobs/sha256/${md#sha256:}"
        cd="$(jq -r '.config.digest' "$out/blobs/sha256/${md#sha256:}")"
        jq -S 'del(.rootfs)' "$out/blobs/sha256/${cd#sha256:}"
    done < <(jq -r '.manifests[].digest' "$out/index.json")
}

printf 'attr  : .#%s\n' "$attr"
printf 'input : %s\n' "$input"
printf 'against: %s\n\n' "$ref"

echo "building both sides (this can take a while)..."
base_out="$(run_quiet nix build --no-link --print-out-paths ".#$attr" ${extra+"${extra[@]}"})"
over_out="$(run_quiet nix build --no-link --print-out-paths --no-write-lock-file \
    --override-input "$input" "$ref" ".#$attr" ${extra+"${extra[@]}"})"

for out in "$base_out" "$over_out"; do
    [ -f "$out/index.json" ] ||
        die "$out is not an OCI layout directory (an oci-archive output cannot be compared this way)"
done

echo
if diff -q <(layers_of "$base_out") <(layers_of "$over_out") >/dev/null; then
    echo "layers: identical — the $input move does not re-compress anything you build."
    if diff -q <(manifests_of "$base_out") <(manifests_of "$over_out") >/dev/null; then
        echo "manifest and config: identical."
        echo
        echo "digest-neutral: nothing needs rebuilding or re-pushing."
        exit 0
    fi
    echo
    echo "manifest and/or config digests DIFFER while every layer is identical."
    echo "That is the measurement, not the toolchain: --override-input makes the"
    echo "flake dirty, so self.rev is unavailable and anything stamped from it"
    echo "moves. What changed:"
    echo
    diff <(contents_of "$base_out") <(contents_of "$over_out") || true
    echo
    echo "If every difference above is a revision or build-metadata field, your"
    echo "images are digest-neutral under this move and the manifest digests"
    echo "will not move in a real build, where self.rev resolves normally."
    exit 0
fi

echo "layers DIFFER — the $input move re-digests images you build:"
diff <(layers_of "$base_out") <(layers_of "$over_out") || true
echo
echo "A *subset* differing is the signature of a source change rather than a"
echo "compressor change: store paths appear inside the layer tar, so a binary"
echo "built from moved source lands on a new path. All layers differing is the"
echo "compressor. Either way, anything pinning these by digest needs updating."
