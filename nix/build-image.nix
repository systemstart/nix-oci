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
  # Remaining runtime-config fields (OCI image config).
  workingDir ? "",
  user ? "",
  exposedPorts ? [ ], # e.g. [ "8080/tcp" ]
  volumes ? [ ], # e.g. [ "/data" ]
  labels ? { }, # config labels, an attrset of KEY = "value"
  stopSignal ? "",
  # Manifest annotations (an attrset of KEY = "value"), e.g.
  # { "org.opencontainers.image.source" = "https://…"; }.
  annotations ? { },
  arch ? "amd64",
  os ? "linux",
  ref ? "latest",
  maxLayers ? 100,
  # Shell run in a staging directory whose contents become a final layer at the
  # image root (e.g. `mkdir etc && echo ... > etc/passwd`). The content is
  # root-owned unless `chown` says otherwise.
  extraCommands ? "",
  # Ownership rules for customization-layer paths -- the fakeRootCommands/chown
  # equivalent. Each is { path; uid; gid; recursive ? false; }, path being
  # root-relative (e.g. "home/nonroot"). The path must exist in the custom layer
  # (created by extraCommands/rootLinks/binLinks); store-path content is always
  # root-owned.
  chown ? [ ],
  # Root-level symlinks, { "<root-relative path>" = "<target>"; }, e.g.
  # { "bin/app" = "${app}/bin/app"; }. Smooths migrations
  # from dockerTools images that hardcode /bin/<name>: nix-oci keeps `contents`
  # at their store paths, so conventional paths otherwise do not exist. The
  # target must be within the closure (a `contents` entry).
  rootLinks ? { },
  # Packages whose entire bin/ is symlinked into the image's /bin (like
  # dockerTools merging a package into /). Each package is also added to the
  # closure, so `binLinks = [ pkg ]` both packages and exposes it.
  binLinks ? [ ],
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
  # binLinks packages must be in the image, so they join the closure roots.
  roots = contents ++ binLinks;
  hasContents = roots != [ ];
  closure = closureInfo { rootPaths = roots; };

  # Render a list-valued flag as the CLI's comma-separated form, or nothing when
  # the list is empty (an empty continuation line is harmless in the script).
  listFlag =
    flag: values:
    lib.optionalString (values != [ ]) "--${flag} ${lib.escapeShellArg (lib.concatStringsSep "," values)}";

  scalarFlag = flag: value: lib.optionalString (value != "") "--${flag} ${lib.escapeShellArg value}";

  # Render an attrset as repeated --flag KEY=VALUE.
  mapFlag =
    flag: attrs:
    lib.concatStringsSep " " (
      lib.mapAttrsToList (k: v: "--${flag} ${lib.escapeShellArg "${k}=${v}"}") attrs
    );

  configFlags = lib.concatStringsSep " " [
    (scalarFlag "working-dir" workingDir)
    (scalarFlag "user" user)
    (listFlag "exposed-ports" exposedPorts)
    (listFlag "volumes" volumes)
    (scalarFlag "stop-signal" stopSignal)
    (mapFlag "label" labels)
    (mapFlag "annotation" annotations)
  ];

  # With no store paths, feed an empty closure and skip the graph.
  storePathsInput = if hasContents then "${closure}/store-paths" else "/dev/null";
  graphFlag = lib.optionalString hasContents "--closure-graph ${closure}/registration";
  fromFlag = lib.optionalString (fromImage != null) "--from-image ${fromImage}";

  # rootLinks/binLinks become `ln -s` commands run in the same staging dir as
  # extraCommands, so all three contribute to one customization layer.
  rootLinkCmds = lib.concatStringsSep "\n" (
    lib.mapAttrsToList (
      name: target:
      "mkdir -p \"$(dirname ${lib.escapeShellArg name})\" && ln -s ${lib.escapeShellArg target} ${lib.escapeShellArg name}"
    ) rootLinks
  );

  # `${pkg}/bin/*` is deliberately unquoted so the shell globs it; store paths
  # never contain spaces. Each entry lands at /bin/<name>.
  binLinkCmds = lib.concatMapStringsSep "\n" (pkg: ''
    mkdir -p bin
    for f in ${pkg}/bin/*; do ln -s "$f" "bin/$(basename "$f")"; done
  '') binLinks;

  # The staging script is the union of the three content sources; the layer
  # exists only if at least one produced something.
  stageScript = lib.concatStringsSep "\n" (
    lib.filter (s: s != "") [
      extraCommands
      rootLinkCmds
      binLinkCmds
    ]
  );
  hasCustom = stageScript != "";

  # Render chown rules as repeated --own PATH:UID:GID[:r].
  ownFlags = lib.concatStringsSep " " (
    map (
      o:
      "--own ${
        lib.escapeShellArg "${o.path}:${toString o.uid}:${toString o.gid}${lib.optionalString (o.recursive or false) ":r"}"
      }"
    ) chown
  );

  # The staging script runs into a build-local staging dir, NOT a separate store
  # path. Materialising it as its own derivation would subject it to Nix store
  # canonicalisation, which strips write/sticky/setuid bits (so `chmod 1777 tmp`
  # would silently become 0555). Staging inside this build and packing it before
  # any output is registered preserves the modes verbatim.
  stageCustom = lib.optionalString hasCustom ''
    custom_root="$(mktemp -d)"
    ( cd "$custom_root" && ${stageScript} )
  '';

  customFlag = lib.optionalString hasCustom ''--custom-layer "$custom_root" ${ownFlags}'';

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
      ${configFlags} \
      ${customFlag} \
      < ${storePathsInput} ${redirect}
  ''
