{
  inputs = {
    # master until nixpkgs-unstable carries Go 1.27.3, which actions/scaleset
    # requires; then back to nixpkgs-unstable.
    nixpkgs.url = "github:NixOS/nixpkgs/master";
    # No x86_64-darwin: nixpkgs-unstable dropped it in 26.11 and throws on
    # evaluation.
    flake-compat.url = "github:edolstra/flake-compat";
    flake-compat.flake = false;
  };

  outputs =
    { self
    , nixpkgs
    , ...
    }:
    let
      version = "0.1.5"; # x-release-please-version
      systems = [
        "aarch64-darwin"
        "aarch64-linux"
        "x86_64-linux"
      ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
    in
    {
      packages = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          runner-up = pkgs.callPackage ./package.nix { inherit version; };
        in
        {
          default = runner-up;
          inherit runner-up;
        }
      );

      apps = forAllSystems (
        system:
        {
          default = {
            type = "app";
            program = "${self.packages.${system}.default}/bin/${self.packages.${system}.default.meta.mainProgram}";
            meta = self.packages.${system}.default.meta;
          };
          runner-up = {
            type = "app";
            program = "${self.packages.${system}.runner-up}/bin/${self.packages.${system}.runner-up.meta.mainProgram}";
            meta = self.packages.${system}.runner-up.meta;
          };
        }
      );
    };
}
