{
  lib,
  buildGoModule,
  version,
}:

buildGoModule {
  pname = "linux-ha-agent";
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

  subPackages = [ "cmd/linux-ha-agent" ];
  env.CGO_ENABLED = 0;
  ldflags = [
    "-s"
    "-w"
    "-X main.version=${version}"
  ];

  # The tests run in CI; some need a network-free sandbox they cannot get here.
  doCheck = false;

  meta = {
    description = "Home Assistant MQTT agent for Linux machines, Incus and Proxmox VE hosts";
    homepage = "https://github.com/lvlie/linux-ha-agent";
    license = lib.licenses.gpl3Plus;
    mainProgram = "linux-ha-agent";
    platforms = lib.platforms.linux;
  };
}
