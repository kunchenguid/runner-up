package release

// signingKeys are the base64 public halves of the release signing keys,
// separated by spaces: Ed25519 keys whose private halves sign each release's
// checksums.txt in the release workflow. runner-up's Windows service installs
// only a release one of them signed, so whoever holds a private half controls
// every Windows machine running the service. The one key's private half is
// only the RELEASE_SIGNING_KEY secret of the repository's release
// environment, which only main reaches; the release workflow checks its
// signature against this line. A string, so a test build can trust a key of
// its own with
// -X github.com/kunchenguid/runner-up/internal/release.signingKeys=KEY.
var signingKeys = "ETVXJEADqxXNJEbNpCs2ceglGJDucN7rEBigoqFX8RY="
