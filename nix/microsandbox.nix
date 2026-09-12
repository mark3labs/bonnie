# microsandbox CLI, packaged from the upstream release bundle.
#
# Upstream ships a prebuilt `msb` binary plus the matching `libkrunfw` shared
# library. We do not build from source: the crate needs a full libkrun and
# libkrunfw toolchain, and upstream only audits the released bundle.
#
# The bundle layout is kept exactly as the upstream installer makes it
# (bin/msb, lib/libkrunfw.so.N), because `msb` dlopens libkrunfw by soname at
# run time.
{
  lib,
  stdenv,
  stdenvNoCC,
  fetchurl,
  autoPatchelfHook,
  makeWrapper,
  libcap_ng,
}:

let
  version = "0.6.18";

  # sha256 values come from the release checksums.sha256 asset.
  sources = {
    "x86_64-linux" = {
      asset = "microsandbox-linux-x86_64.tar.gz";
      hash = "sha256-sAGzxrmAqx/8zrgXSWZIwVINuja54MqsN+qNL0rNm90=";
    };
    "aarch64-linux" = {
      asset = "microsandbox-linux-aarch64.tar.gz";
      hash = "sha256-5TCY52Af3dha9+lD1K89Q3Cs4nbZ6GOolFYzjy0HbRs=";
    };
    "aarch64-darwin" = {
      asset = "microsandbox-darwin-aarch64.tar.gz";
      hash = "sha256-HoxAhZFCzTj7mbMBvbH7QJWphaQGXQgPmbOj58uaYwU=";
    };
  };

  system = stdenvNoCC.hostPlatform.system;

  source = sources.${system} or (throw "bonnie: microsandbox has no upstream release for ${system}");
in
stdenvNoCC.mkDerivation {
  pname = "microsandbox";
  inherit version;

  src = fetchurl {
    url = "https://github.com/superradcompany/microsandbox/releases/download/v${version}/${source.asset}";
    inherit (source) hash;
  };

  sourceRoot = ".";

  nativeBuildInputs = [
    makeWrapper
  ]
  ++ lib.optionals stdenv.hostPlatform.isLinux [ autoPatchelfHook ];

  buildInputs = lib.optionals stdenv.hostPlatform.isLinux [
    libcap_ng
    stdenv.cc.cc.lib
  ];

  # `msb` dlopens libkrunfw.so.5 by soname, so $out/lib must be on its
  # RUNPATH. The RUNPATH upstream ships is a literal "\$$ORIGIN/../lib",
  # which the loader never expands.
  appendRunpaths = [ "${placeholder "out"}/lib" ];

  dontConfigure = true;
  dontBuild = true;

  installPhase = ''
    runHook preInstall

    install -Dm755 msb "$out/bin/msb"

    # Upstream also exposes the binary as `microsandbox`.
    ln -s msb "$out/bin/microsandbox"

    mkdir -p "$out/lib"
  ''
  + lib.optionalString stdenv.hostPlatform.isLinux ''
    krunfw=$(echo libkrunfw.so.*.*.*)
    abi=''${krunfw#libkrunfw.so.}
    abi=''${abi%%.*}
    install -Dm644 "$krunfw" "$out/lib/$krunfw"
    ln -s "$krunfw" "$out/lib/libkrunfw.so.$abi"
    ln -s "libkrunfw.so.$abi" "$out/lib/libkrunfw.so"
  ''
  + lib.optionalString stdenv.hostPlatform.isDarwin ''
    krunfw=$(echo libkrunfw.*.dylib)
    install -Dm644 "$krunfw" "$out/lib/$krunfw"
    ln -s "$krunfw" "$out/lib/libkrunfw.dylib"
  ''
  + ''

    # MSB_LIBKRUNFW_PATH is the documented escape hatch. Set it as a default
    # so the library is found even if the loader ignores RUNPATH, but let the
    # caller override it.
    wrapProgram "$out/bin/msb" \
      --set-default MSB_LIBKRUNFW_PATH "$out/lib/$krunfw"

    runHook postInstall
  '';

  meta = {
    description = "Self-hosted sandbox that runs untrusted code in hardware-isolated microVMs";
    homepage = "https://github.com/superradcompany/microsandbox";
    license = lib.licenses.asl20;
    mainProgram = "msb";
    platforms = lib.attrNames sources;
    sourceProvenance = [ lib.sourceTypes.binaryNativeCode ];
  };
}
