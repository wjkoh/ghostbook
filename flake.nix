{
  description = "Ghostbook: Single-binary collaborative living book where humans read cleanly on the web and only AI agents write";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs = {
    self,
    nixpkgs,
  }: let
    supportedSystems = ["x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin"];
    forEachSupportedSystem = f:
      nixpkgs.lib.genAttrs supportedSystems (system:
        f {
          pkgs = import nixpkgs {inherit system;};
          inherit system;
        });
    version = "0.1.6";
  in {
    devShells = forEachSupportedSystem ({pkgs, ...}: {
      default = pkgs.mkShell {
        packages = with pkgs; [
          go
          go-tools
          buf
          git
        ];
      };
    });

    packages = forEachSupportedSystem ({pkgs, ...}: let
      ghostbook = pkgs.buildGoModule {
        pname = "ghostbook";
        inherit version;
        src = ./.;
        vendorHash = "sha256-lUYKdJoSX5+PzJpYpnG1ZAn1pnPoqGWpNChs6oEWGVQ=";

        env = {
          CGO_ENABLED = "0";
        };

        subPackages = ["cmd/ghostbook"];

        nativeCheckInputs = [pkgs.git];

        ldflags = [
          "-s"
          "-w"
          "-X main.version=v${version}"
        ];

        checkFlags = [
          "-skip=TestBrowserEndToEnd|TestDemoVisualAudit"
        ];

        meta = {
          description = "Single-binary Git-backed collaborative living book studio where only AI agents write";
          homepage = "https://github.com/wjkoh/ghostbook";
          license = pkgs.lib.licenses.asl20;
          mainProgram = "ghostbook";
          platforms = supportedSystems;
        };
      };
    in {
      default = ghostbook;
      inherit ghostbook;
    });

    apps = forEachSupportedSystem ({system, ...}: {
      default = {
        type = "app";
        program = "${self.packages.${system}.default}/bin/ghostbook";
        meta = self.packages.${system}.default.meta;
      };
    });
  };
}
