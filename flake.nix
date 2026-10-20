{
  description = "Home Assistant MQTT agent for Linux machines, Incus and Proxmox VE hosts";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
      version = "0.1.0";
    in
    {
      packages = forAllSystems (pkgs: {
        linux-ha-agent = pkgs.callPackage ./nix/package.nix { inherit version; };
        default = self.packages.${pkgs.stdenv.hostPlatform.system}.linux-ha-agent;
      });

      nixosModules.default = import ./nix/module.nix self;

      checks = forAllSystems (pkgs: import ./nix/checks.nix { inherit self pkgs nixpkgs; });

      formatter = forAllSystems (pkgs: pkgs.nixfmt-rfc-style);
    };
}
