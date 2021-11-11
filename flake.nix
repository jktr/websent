{
  description = "A tool for quick and easy presentations";

  outputs = { self, nixpkgs }: {

    overlay = final: prev: {
      websent = final.buildGoModule {
        name = "websent";
        src = self;
        vendorSha256 = "sha256-urWsOTiRQgMHgLOrio3nLgUguqOkqi55II8XLnR46F8=";
      };
    };

    packages.x86_64-linux.websent = (import nixpkgs {
      system = "x86_64-linux";
      overlays = [ self.overlay ];
    }).websent;

    defaultPackage.x86_64-linux = self.packages.x86_64-linux.websent;
  };
}
