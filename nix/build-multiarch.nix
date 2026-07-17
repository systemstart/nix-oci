# buildOCIMultiArch: tie several single-platform images into one multi-arch
# image (an OCI image index over per-platform manifests).
#
# Each entry in `images` is a layout built by buildOCIImage for one platform
# (typically over a pkgsCross closure with a matching `arch`). Because those are
# independent derivations, remote builders can produce them in parallel; this
# step is the cheap finale -- it only unions the blobs and rewrites the index.
{
  lib,
  runCommand,
  nix-oci,
}:

{
  name,
  # List of single-platform layout derivations (from buildOCIImage).
  images,
  # "directory" (an OCI image layout) or "archive" (a single oci-archive tar).
  format ? "directory",
}:

assert lib.elem format [
  "directory"
  "archive"
];

let
  outputFlag = if format == "archive" then "--archive" else ''--output "$out"'';
  redirect = lib.optionalString (format == "archive") ''> "$out"'';
in
runCommand name
  {
    nativeBuildInputs = [ nix-oci ];
  }
  ''
    nix-oci combine ${outputFlag} ${lib.escapeShellArgs images} ${redirect}
  ''
