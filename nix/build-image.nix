# buildOCIImage: express an OCI image as a derivation.
#
# This is the closure-in/layout-out boundary the CLI documents, but computed by
# Nix instead of a shell pipe. `closureInfo` derives the runtime closure of
# `contents` and emits a `store-paths` file -- one path per line -- which is
# exactly what `nix-oci build` reads on stdin. Because closureInfo's output
# references every path in the closure, depending on it drags the whole closure
# into the build sandbox, so the writer can read each store path's bytes.
{
  lib,
  runCommand,
  closureInfo,
  nix-oci,
}:

{
  name,
  # Derivations or store paths to package (the image's roots). May be empty when
  # `fromImage` and/or `extraCommands` supply the content.
  contents ? [ ],
  entrypoint ? [ ],
  cmd ? [ ],
  env ? [ ],
  arch ? "amd64",
  os ? "linux",
  ref ? "latest",
  maxLayers ? 100,
  # Shell run in a staging directory whose contents become a final layer at the
  # image root (e.g. `mkdir etc && echo ... > etc/passwd`). The content is
  # root-owned in the image.
  extraCommands ? "",
  # A base OCI image layout to layer on top of (the fromImage feature). Our
  # layers sit above the base's, and our config inherits the base's (overriding
  # entrypoint/cmd/env/workingDir).
  fromImage ? null,
  # "directory" (an OCI image layout) or "archive" (a single oci-archive tar,
  # streamed to the output).
  format ? "directory",
}:

assert lib.elem format [
  "directory"
  "archive"
];

let
  hasContents = contents != [ ];
  closure = closureInfo { rootPaths = contents; };

  # Render a list-valued flag as the CLI's comma-separated form, or nothing when
  # the list is empty (an empty continuation line is harmless in the script).
  listFlag =
    flag: values:
    lib.optionalString (values != [ ]) "--${flag} ${lib.escapeShellArg (lib.concatStringsSep "," values)}";

  # With no store paths, feed an empty closure and skip the graph.
  storePathsInput = if hasContents then "${closure}/store-paths" else "/dev/null";
  graphFlag = lib.optionalString hasContents "--closure-graph ${closure}/registration";
  fromFlag = lib.optionalString (fromImage != null) "--from-image ${fromImage}";

  # extraCommands runs into a build-local staging dir, NOT a separate store path.
  # Materialising it as its own derivation would subject it to Nix store
  # canonicalisation, which strips write/sticky/setuid bits (so `chmod 1777 tmp`
  # would silently become 0555). Staging inside this build and packing it before
  # any output is registered preserves the modes verbatim.
  stageCustom = lib.optionalString (extraCommands != "") ''
    custom_root="$(mktemp -d)"
    ( cd "$custom_root" && ${extraCommands} )
  '';

  customFlag = lib.optionalString (extraCommands != "") ''--custom-layer "$custom_root"'';

  # A directory layout writes into $out; an archive streams to stdout, which we
  # redirect to $out.
  outputFlag = if format == "archive" then "--archive" else ''--output "$out"'';
  redirect = lib.optionalString (format == "archive") ''> "$out"'';
in
runCommand name
  (
    {
      nativeBuildInputs = [ nix-oci ];
    }
    // lib.optionalAttrs hasContents { inherit closure; }
  )
  ''
    ${stageCustom}
    nix-oci build \
      ${outputFlag} \
      --arch ${lib.escapeShellArg arch} \
      --os ${lib.escapeShellArg os} \
      --ref ${lib.escapeShellArg ref} \
      --max-layers ${toString maxLayers} \
      ${graphFlag} \
      ${fromFlag} \
      ${listFlag "entrypoint" entrypoint} \
      ${listFlag "cmd" cmd} \
      ${listFlag "env" env} \
      ${customFlag} \
      < ${storePathsInput} ${redirect}
  ''
