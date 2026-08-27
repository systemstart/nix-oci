{
  description = "nix-oci: a native OCI image writer for Nix";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      packages = forAllSystems (pkgs:
        {
          default = self.packages.${pkgs.stdenv.hostPlatform.system}.nix-oci;

          nix-oci =
            let
              # Version is the single source of truth in ./VERSION, bumped and
              # committed by `make release-tag` as part of tagging -- so the Nix
              # build, the goreleaser artifact, and the git tag can never drift.
              # It names the Nix package and stamps the binary (via ldflags).
              version = pkgs.lib.fileContents ./VERSION;
            in
            # Pinned to a Go *minor*, not the default toolchain: gzip at
            # BestCompression makes the compressor version digest-affecting
            # (see DESIGN.md), and compress/flate output has changed between
            # minors before. Patch bumps within 1.26 are taken as they land.
            (pkgs.buildGoModule.override { go = pkgs.go_1_26; }) {
              pname = "nix-oci";
              inherit version;
              src = ./.;
              vendorHash = "sha256-RCPLKumngkBa+2p/d30kFKkcfOitEw6p+ECC2k9Lg10=";

              ldflags = [
                "-s"
                "-w"
                "-X"
                "main.version=${version}"
              ];

              # Kept in sync with pkgs/by-name/ni/nix-oci/package.nix in nixpkgs.
              # `description` deliberately has no leading article: nixpkgs' meta
              # lint rejects one.
              meta = {
                description = "Native, deterministic OCI image layout writer for Nix";
                homepage = "https://github.com/systemstart/nix-oci";
                changelog = "https://github.com/systemstart/nix-oci/releases/tag/v${version}";
                license = pkgs.lib.licenses.gpl3Only;
                mainProgram = "nix-oci";
                platforms = pkgs.lib.platforms.unix;
              };
            };
        });

      # buildOCIImage is a function, so it cannot live under `packages` (which
      # must contain derivations). legacyPackages is the escape hatch.
      legacyPackages = forAllSystems (pkgs: {
        buildOCIImage = pkgs.callPackage ./nix/build-image.nix {
          nix-oci = self.packages.${pkgs.stdenv.hostPlatform.system}.nix-oci;
        };

        buildOCIMultiArch = pkgs.callPackage ./nix/build-multiarch.nix {
          nix-oci = self.packages.${pkgs.stdenv.hostPlatform.system}.nix-oci;
        };

        buildOCIImageCached = pkgs.callPackage ./nix/build-image-cached.nix {
          nix-oci = self.packages.${pkgs.stdenv.hostPlatform.system}.nix-oci;
        };
      });

      # Worked examples of every image-building function. They live under `checks`
      # (not `packages`) so `nix flake check` builds them as living, verified
      # documentation without cluttering the public package set.
      checks = forAllSystems (pkgs:
        let
          lib' = self.legacyPackages.${pkgs.stdenv.hostPlatform.system};
        in
        {
          # A closure plus a customization layer (/etc, sticky /tmp), runtime
          # config (user, ports, labels), provenance annotations, and the
          # migration conveniences: /bin/hello via binLinks, a /bin/greet alias
          # via rootLinks, and a non-root-owned home via chown.
          exampleImage = lib'.buildOCIImage {
            name = "hello-oci";
            contents = [ pkgs.hello ];
            entrypoint = [ "/bin/hello" ];
            user = "1000:1000";
            exposedPorts = [ "8080/tcp" ];
            labels."org.opencontainers.image.title" = "hello";
            annotations."org.opencontainers.image.source" = "https://github.com/systemstart/nix-oci";
            binLinks = [ pkgs.hello ];
            rootLinks."bin/greet" = "${pkgs.hello}/bin/hello";
            extraCommands = ''
              mkdir -p etc tmp home/nonroot
              echo 'root:x:0:0:root:/root:/bin/sh' > etc/passwd
              chmod 1777 tmp
            '';
            chown = [
              {
                path = "home/nonroot";
                uid = 1000;
                gid = 1000;
                recursive = true;
              }
            ];
          };

          # The same image as a single streamed oci-archive tar.
          exampleArchive = lib'.buildOCIImage {
            name = "hello-oci.tar";
            format = "archive";
            contents = [ pkgs.hello ];
            entrypoint = [ "${pkgs.hello}/bin/hello" ];
          };

          # fromImage: layer on top of an existing base image (here, the
          # exampleImage layout), adding only a customization layer. The base's
          # layers and config are inherited.
          exampleFromImage = lib'.buildOCIImage {
            name = "hello-on-base";
            fromImage = self.checks.${pkgs.stdenv.hostPlatform.system}.exampleImage;
            extraCommands = ''
              mkdir -p etc
              echo 'layered on a base image' > etc/note
            '';
          };

          # Explicitly layered and cached: glibc as a stable base, hello on top.
          exampleImageCached = lib'.buildOCIImageCached {
            name = "hello-oci-cached";
            contents = [ pkgs.hello ];
            layers = [ [ pkgs.glibc ] ];
            entrypoint = [ "${pkgs.hello}/bin/hello" ];
          };

          # A multi-arch image index.
          #
          # NOTE: for illustration both entries package the *native* hello, so
          # the arm64 entry is not runnable on arm64 -- it only shows the index
          # assembly. A real multi-arch image passes a pkgsCross closure per
          # platform (see the README).
          exampleMultiArch =
            let
              image = arch: lib'.buildOCIImage {
                name = "hello-${arch}";
                contents = [ pkgs.hello ];
                inherit arch;
                entrypoint = [ "${pkgs.hello}/bin/hello" ];
              };
            in
            lib'.buildOCIMultiArch {
              name = "hello-multiarch";
              images = [ (image "amd64") (image "arm64") ];
            };
        });

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = (with pkgs; [
            # Same minor the derivation builds with -- the compressor version
            # is digest-affecting, so developing against a different Go would
            # produce layers that do not match a Nix build.
            go_1_26
            gopls
            gotools

            gnumake
            golangci-lint # v2 -- matches `version: "2"` in .golangci.yaml
            gofumpt

            # Release tooling (see `make release` / `make release-tag`).
            goreleaser
            (pkgs.callPackage ./nix/gsemver.nix { })

            # Conformance oracles and consumers (see the consumption matrix).
            skopeo
            umoci
            crane

            jq

            # Compressor versions are digest-affecting: the writer must be
            # developed against the same builds the derivation uses.
            gzip
            zstd
          ]);
        };
      });
    };
}
