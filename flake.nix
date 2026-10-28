{
  description = "berth — stable ports and process hygiene for coding agents and git worktrees";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAll = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      packages = forAll (pkgs: rec {
        berth = pkgs.buildGoModule {
          pname = "berth";
          version = self.shortRev or "dev";
          src = ./.;
          vendorHash = "sha256-R2ZdV4TED0zp/nQUcxo1uvXQxPljlRg5CCCiwN6+3aY=";
          ldflags = [ "-s" "-w" "-X github.com/Fahnoee/berth/internal/cli.Version=${self.shortRev or "dev"}" ];
          meta = {
            description = "Stable ports and process hygiene for coding agents and git worktrees";
            homepage = "https://github.com/Fahnoee/berth";
            license = pkgs.lib.licenses.mit;
            mainProgram = "berth";
          };
        };
        default = berth;
      });

      devShells = forAll (pkgs: {
        default = pkgs.mkShell { packages = [ pkgs.go pkgs.gopls ]; };
      });
    };
}
