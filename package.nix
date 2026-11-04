{ lib
, buildGo127Module
, version ? "0.1.12" # x-release-please-version
}:

buildGo127Module {
  pname = "runner-up";
  inherit version;

  src = lib.cleanSource ./.;
  vendorHash = "sha256-SudR8inORWnF/fKHl9F7oQh5l9p7/VCiIDYVvzephXg=";

  subPackages = [ "." ];

  ldflags = [
    "-X github.com/kunchenguid/runner-up/internal/version.Version=v${version}"
  ];

  # The tests build runner-up and run it as a program beside stand-in ssh,
  # docker, tart and runner commands, which the Nix sandbox does not provide.
  # The project's own CI runs `go test ./...` on ubuntu, macOS and Windows.
  doCheck = false;

  meta = {
    description = "Run GitHub Actions jobs on your own machines, one fresh container or VM per job";
    homepage = "https://github.com/kunchenguid/runner-up";
    license = lib.licenses.mit;
    mainProgram = "runner-up";
    platforms = [
      "aarch64-darwin"
      "aarch64-linux"
      "x86_64-linux"
    ];
  };
}
