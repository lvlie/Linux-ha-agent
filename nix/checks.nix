{
  self,
  pkgs,
  nixpkgs,
}:

let
  system = pkgs.stdenv.hostPlatform.system;
  machine = nixpkgs.lib.nixosSystem {
    inherit system;
    modules = [
      self.nixosModules.default
      {
        boot.isContainer = true;
        system.stateVersion = "25.11";
        services.incus-ha-agent = {
          enable = true;
          settings = {
            mqtt.url = "tcp://mqtt.lan:1883";
            upgrade.schedule = "30 3 * * 0";
          };
        };
      }
    ];
  };
  service = machine.config.systemd.services.incus-ha-agent;
in
{
  package = self.packages.${system}.default;

  # Evaluates the module and builds the validated config file.
  module = pkgs.runCommand "incus-ha-agent-module-check" { } ''
    exec_start=${pkgs.lib.escapeShellArg service.serviceConfig.ExecStart}
    echo "$exec_start" | grep -q -- '-config /nix/store/.*incus-ha-agent.yaml'
    grep -q 'state_file: /var/lib/incus-ha-agent/state.json' ${builtins.elemAt (pkgs.lib.splitString " " service.serviceConfig.ExecStart) 2}
    touch $out
  '';
}
