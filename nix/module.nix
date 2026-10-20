self:
{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.services.incus-ha-agent;
  format = pkgs.formats.yaml { };
  rawConfig = format.generate "incus-ha-agent.yaml" cfg.settings;
  # Validate the configuration when the system is built, not when it boots.
  configFile = pkgs.runCommand "incus-ha-agent.yaml" { } ''
    ${lib.getExe cfg.package} -config ${rawConfig} -check-config
    cp ${rawConfig} $out
  '';
in
{
  options.services.incus-ha-agent = {
    enable = lib.mkEnableOption "the Home Assistant agent for Incus hosts";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      defaultText = lib.literalExpression "incus-ha-agent.packages.\${system}.default";
      description = "The incus-ha-agent package to use.";
    };

    settings = lib.mkOption {
      inherit (format) type;
      default = { };
      example = lib.literalExpression ''
        {
          mqtt = {
            url = "ssl://mqtt.lan:8883";
            username = config.networking.hostName;
            password_file = "/run/secrets/incus-ha-agent-mqtt";
          };
          upgrade.schedule = "30 3 * * 0";
          nixos.flake = "github:me/nix-config";
        }
      '';
      description = ''
        Agent configuration, see config.example.yaml in the repository. Keep
        secrets out of the Nix store: use `mqtt.password_file`.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    services.incus-ha-agent.settings.state_file = lib.mkDefault "/var/lib/incus-ha-agent/state.json";

    systemd.services.incus-ha-agent = {
      description = "Home Assistant agent for Incus hosts and Linux machines";
      documentation = [ "https://github.com/lvlie/incus-ha-agent" ];
      wantedBy = [ "multi-user.target" ];
      wants = [ "network-online.target" ];
      after = [
        "network-online.target"
        "incus.service"
        "docker.service"
        "podman.service"
      ];
      # Upgrading the system can restart this service; keep a running
      # nixos-rebuild alive instead of killing it with the agent.
      restartIfChanged = false;
      serviceConfig = {
        ExecStart = "${lib.getExe cfg.package} -config ${configFile}";
        Restart = "on-failure";
        RestartSec = 10;
        KillMode = "process";
        StateDirectory = "incus-ha-agent";
        StateDirectoryMode = "0700";
      };
    };
  };
}
