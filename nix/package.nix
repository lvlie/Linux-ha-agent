{
  lib,
  buildGoModule,
  version,
}:

buildGoModule {
  pname = "incus-ha-agent";
  inherit version;

  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../go.mod
      ../go.sum
      ../cmd
      ../internal
    ];
  };

  # Update after changing go.mod: set to lib.fakeHash, build, copy the "got" hash.
  vendorHash = "sha256-QG3WkMq4hhoJw5DEDDMaDO/2Pur92tjBF2GJxAZ2tFo=";

  subPackages = [ "cmd/incus-ha-agent" ];
  env.CGO_ENABLED = 0;
  ldflags = [
    "-s"
    "-w"
    "-X main.version=${version}"
  ];

  # The tests run in CI; some need a network-free sandbox they cannot get here.
  doCheck = false;

  meta = {
    description = "Home Assistant MQTT agent for Incus hosts and Linux machines";
    homepage = "https://github.com/lvlie/incus-ha-agent";
    license = lib.licenses.gpl3Plus;
    mainProgram = "incus-ha-agent";
    platforms = lib.platforms.linux;
  };
}
