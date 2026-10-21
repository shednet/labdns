{
  description = "labdns development shell";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
    }:
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = import nixpkgs {
          inherit system;
        };
        buildGo127Module = pkgs.buildGoModule.override { go = pkgs.go_1_27; };
        cliVersion = self.shortRev or "dev";
        labdns = buildGo127Module {
          pname = "labdns";
          version = cliVersion;
          src = self;
          subPackages = [ "cmd/labdns" ];
          vendorHash = "sha256-JWRWe43Bf4tZ5zB+os+BUHWvqH/x2chil21l2l7FVX0=";
          ldflags = [ "-s" "-w" "-X main.version=${cliVersion}" ];
          nativeBuildInputs = [ pkgs.installShellFiles ];
          postInstall = ''
            installShellCompletion --cmd labdns \
              --bash <($out/bin/labdns completion bash) \
              --fish <($out/bin/labdns completion fish) \
              --zsh <($out/bin/labdns completion zsh)
          '';
        };
      in
      {
        packages.labdns = labdns;
        packages.default = labdns;
        apps.labdns = flake-utils.lib.mkApp { drv = labdns; };
        apps.default = flake-utils.lib.mkApp { drv = labdns; };

        devShells.default = pkgs.mkShell {
          packages = with pkgs; [
            go_1_27
            kind
            kubernetes-helm
          ];

          shellHook = ''
            export PATH="$PWD/bin:$PATH"
          '';
        };
      }
    );
}
