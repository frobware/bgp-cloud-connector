{
  description = "Development shell for bgp-cloud-connector";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          name = "bgp-cloud-connector";

          # The Makefile installs its own pinned controller-gen,
          # kustomize, setup-envtest and golangci-lint into ./bin, so
          # what you generate locally matches what CI generates. All
          # this shell owes it is a Go toolchain and a C compiler: go
          # install defaults to CGO_ENABLED=1 and fails outright
          # without one.
          packages = with pkgs; [
            go_1_26
            gcc
            gnumake
            git
            kubectl
            openshift # oc
          ];
        };
      });

      formatter = forAllSystems (pkgs: pkgs.nixpkgs-fmt);
    };
}
