# Go 1.26.5 from the official prebuilt tarballs.
#
# nixpkgs (master, nixpkgs-unstable and nixos-unstable alike) is still on
# 1.26.4, so there is no input bump that reaches 1.26.5. This derivation follows
# the same shape as nixpkgs' own prebuilt-Go bootstrap
# (pkgs/development/compilers/go/binary.nix): Go's distributed toolchain is
# statically linked, so it needs no patchelf, just unpacking.
#
# Hashes are the sha256 values published in https://go.dev/dl/?mode=json.
{
  lib,
  stdenv,
  fetchurl,
}:
let
  version = "1.26.5";

  hashes = {
    linux-amd64 = "5c2c3b16caefa1d968a94c1daca04a7ca301a496d9b086e17ad77bb81393f053";
    linux-arm64 = "fe4789e92b1f33358680864bbe8704289e7bb5fc207d80623c308935bd696d49";
    darwin-amd64 = "6231d8d3b8f5552ec6cbf6d685bdd5482e1e703214b120e89b3bf0d7bf1ef725";
    darwin-arm64 = "efb87ff28af9a188d0536ef5d42e63dd52ba8263cd7344a993cc48dd11dedb6a";
  };

  inherit (stdenv.hostPlatform.go) GOOS GOARCH;
  platform = "${GOOS}-${GOARCH}";
in
stdenv.mkDerivation {
  pname = "go";
  inherit version;

  src = fetchurl {
    url = "https://go.dev/dl/go${version}.${platform}.tar.gz";
    sha256 =
      hashes.${platform} or (throw "no Go ${version} tarball hash recorded for platform ${platform}");
  };

  # Ship upstream's bytes unmodified: stripping would gain us nothing and would
  # break the code signature on Darwin.
  dontStrip = true;

  installPhase = ''
    runHook preInstall
    mkdir -p $out/share/go $out/bin
    cp -r . $out/share/go
    ln -s $out/share/go/bin/go $out/bin/go
    ln -s $out/share/go/bin/gofmt $out/bin/gofmt
    runHook postInstall
  '';

  passthru = {
    inherit GOOS GOARCH;
    # nix-oci is pure Go (archive/tar, compress/gzip, crypto/sha256). Disabling
    # cgo keeps the writer statically linked and sidesteps the ld.so patching
    # that nixpkgs' from-source Go carries for cgo builds -- patches this
    # prebuilt toolchain does not have.
    CGO_ENABLED = 0;
  };

  meta = {
    description = "Go programming language (official prebuilt toolchain)";
    homepage = "https://go.dev/";
    changelog = "https://go.dev/doc/devel/release#go${lib.versions.majorMinor version}";
    license = lib.licenses.bsd3;
    sourceProvenance = [ lib.sourceTypes.binaryNativeCode ];
    platforms = [
      "x86_64-linux"
      "aarch64-linux"
      "x86_64-darwin"
      "aarch64-darwin"
    ];
    mainProgram = "go";
  };
}
