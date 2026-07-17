# buildOCIImageCached: like buildOCIImage, but with explicit, cached layers.
#
# You name the layer boundaries; each becomes its own derivation, so an
# unchanged layer is substituted rather than recompressed. The classic split is
# a stable base and a volatile app: change the app and only the top layer
# rebuilds, while the base layer stays cached.
#
# Unlike the automatic path, this is pure and needs no experimental Nix features
# -- the layering is given as derivations (context intact), not read back from a
# computed plan. The trade is that you choose the boundaries; there is no
# popularity ranking. The output is a standard OCI layout, byte-identical to what
# buildOCIImage would produce for the same partition.
{
  lib,
  runCommand,
  closureInfo,
  nix-oci,
}:

{
  name,
  # The image content (its closure is the whole image), like buildOCIImage.
  contents,
  # Lower layers, bottom to top: a list of derivation-lists. Each becomes a
  # cached layer holding that closure minus everything already in the layers
  # below it. Everything in `contents` not covered by these goes in the top
  # layer.
  layers ? [ ],
  entrypoint ? [ ],
  cmd ? [ ],
  env ? [ ],
  workingDir ? "",
  user ? "",
  exposedPorts ? [ ],
  volumes ? [ ],
  labels ? { },
  stopSignal ? "",
  annotations ? { },
  arch ? "amd64",
  os ? "linux",
  ref ? "latest",
  extraCommands ? "",
}:

let
  # One cached layer: the closure of `deps` minus the paths in `below` (already
  # built layer derivations). Writes $out/paths (for upper layers to subtract),
  # plus the blob and meta.json from build-layer.
  #
  # The derivation name is deliberately independent of the image `name`: a base
  # layer built from the same deps is then one derivation shared across every
  # image that uses it (and substitutable from a binary cache), not rebuilt per
  # image.
  mkLayer =
    suffix: deps: below:
    let
      ci = closureInfo { rootPaths = deps; };
      belowPaths = lib.concatMapStringsSep " " (l: "${l}/paths") below;
    in
    runCommand "oci-layer-${suffix}" { nativeBuildInputs = [ nix-oci ]; inherit ci; } ''
      mkdir -p "$out"
      cat ${belowPaths} /dev/null | sort -u > below.txt
      comm -23 <(sort "$ci/store-paths") below.txt > "$out/paths"
      nix-oci build-layer --output "$out" < "$out/paths"
    '';

  # Lower layers, folded so each subtracts the ones beneath it.
  lowerLayers = lib.foldl' (
    built: deps: built ++ [ (mkLayer (toString (builtins.length built)) deps built) ]
  ) [ ] layers;

  # The top layer holds the rest of the image content, above the lower layers.
  topLayer = mkLayer "top" contents lowerLayers;

  allLayers = lowerLayers ++ [ topLayer ];

  listFlag =
    flag: values:
    lib.optionalString (values != [ ]) "--${flag} ${lib.escapeShellArg (lib.concatStringsSep "," values)}";

  scalarFlag = flag: value: lib.optionalString (value != "") "--${flag} ${lib.escapeShellArg value}";

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

  stageCustom = lib.optionalString (extraCommands != "") ''
    custom_root="$(mktemp -d)"
    ( cd "$custom_root" && ${extraCommands} )
  '';
  customFlag = lib.optionalString (extraCommands != "") ''--custom-layer "$custom_root"'';
in
runCommand name { nativeBuildInputs = [ nix-oci ]; } ''
  ${stageCustom}
  nix-oci assemble \
    --output "$out" \
    --arch ${lib.escapeShellArg arch} \
    --os ${lib.escapeShellArg os} \
    --ref ${lib.escapeShellArg ref} \
    ${listFlag "entrypoint" entrypoint} \
    ${listFlag "cmd" cmd} \
    ${listFlag "env" env} \
    ${configFlags} \
    ${customFlag} \
    ${lib.escapeShellArgs allLayers}
''
