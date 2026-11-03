package release

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// signedFixture is a release-shaped set made with openssl, as the release
// workflow signs (openssl pkeyutl -sign -rawin), with a throwaway key whose
// public half is test-key.pub: checksums.txt as sha256sum prints it, its
// signature, and the two archives it lists.
func signedFixture(t *testing.T) (Signed, []ed25519.PublicKey) {
	t.Helper()
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	pub, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(read("test-key.pub"))))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		t.Fatalf("test-key.pub: %v", err)
	}
	name := "runner-up-v9.0.2-windows-amd64.zip"
	return Signed{Name: name, Archive: read(name), Checksums: read("checksums.txt"), Signature: read("checksums.txt.sig")},
		[]ed25519.PublicKey{pub}
}

// Intent (the captain's choice: "The service updates itself from signed
// runner-up releases; you create one signing key, and whoever holds it
// controls the PC"): the Windows service accepts a release whose checksums
// the release signing key signed with openssl, as the release workflow does,
// and only for its own platform; it learns the version from the archive.
func TestASignedReleaseIsAcceptedForItsOwnPlatform(t *testing.T) {
	s, keys := signedFixture(t)
	v, err := s.Verify("windows-amd64", keys)
	if err != nil || v != "v9.0.2" {
		t.Fatalf("Verify = %q, %v; want v9.0.2", v, err)
	}
	if _, err := s.Verify("windows-arm64", keys); err == nil {
		t.Fatal("an amd64 archive was accepted for arm64")
	}
}

// Intent (the same choice: only the key's holder controls the PC): anything
// the key did not vouch for is refused - another key, changed checksums, a
// changed archive, an archive the checksums do not list, and every release
// when the program was built with no key at all.
func TestAnythingTheSigningKeyDidNotVouchForIsRefused(t *testing.T) {
	s, keys := signedFixture(t)
	other, _, _ := ed25519.GenerateKey(nil)
	cases := map[string]func(Signed) (Signed, []ed25519.PublicKey){
		"another key": func(s Signed) (Signed, []ed25519.PublicKey) { return s, []ed25519.PublicKey{other} },
		"no key":      func(s Signed) (Signed, []ed25519.PublicKey) { return s, nil },
		"changed checksums": func(s Signed) (Signed, []ed25519.PublicKey) {
			s.Checksums = bytes.Replace(s.Checksums, []byte("1a2d"), []byte("1a2e"), 1)
			return s, keys
		},
		"changed archive": func(s Signed) (Signed, []ed25519.PublicKey) {
			s.Archive = append(bytes.Clone(s.Archive), 0)
			return s, keys
		},
		"unlisted archive": func(s Signed) (Signed, []ed25519.PublicKey) {
			s.Name = "runner-up-v9.0.3-windows-amd64.zip"
			return s, keys
		},
		"not an archive name": func(s Signed) (Signed, []ed25519.PublicKey) {
			s.Name = "checksums.txt"
			return s, keys
		},
	}
	for name, change := range cases {
		c, k := change(s)
		if v, err := c.Verify("windows-amd64", k); err == nil {
			t.Errorf("%s: accepted as %s", name, v)
		}
	}
	if _, err := s.Verify("windows-amd64", nil); err != ErrNoSigningKey {
		t.Errorf("with no key: %v, want ErrNoSigningKey", err)
	}
}

// Intent (the setup names a version and a per-platform SHA-256 the release
// lists): the program comes out of each platform's archive as the release
// workflow packs it - runner-up in a tar.gz, runner-up.exe in a zip.
func TestTheProgramComesOutOfEachPlatformsArchive(t *testing.T) {
	s, _ := signedFixture(t)
	exe, err := Program(s.Archive, "windows-amd64")
	if err != nil || !strings.HasPrefix(string(exe), "MZ") {
		t.Fatalf("windows program = %q, %v", exe, err)
	}
	tgz, err := os.ReadFile(filepath.Join("testdata", "runner-up-v9.0.2-linux-amd64.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	sums, err := ParseChecksums(s.Checksums)
	if err != nil || sums["runner-up-v9.0.2-linux-amd64.tar.gz"] != SHA256(tgz) {
		t.Fatalf("checksums %v do not list the tar.gz's %s (%v)", sums, SHA256(tgz), err)
	}
	prog, err := Program(tgz, "linux-amd64")
	if err != nil || !strings.Contains(string(prog), "v9.0.2") {
		t.Fatalf("linux program = %q, %v", prog, err)
	}
	if _, err := Program(tgz, "windows-amd64"); err == nil {
		t.Fatal("a tar.gz was read as a Windows archive")
	}
}

// Intent (the Windows service never installs an older release, so an old
// signed release with a known flaw cannot be pushed to it): versions order as
// semantic versions, a pre-release before its release.
func TestReleaseVersionsOrderAsSemanticVersions(t *testing.T) {
	ordered := []string{"v0.1.9", "v0.1.10", "v0.2.0-alpha.2", "v0.2.0-alpha.10", "v0.2.0-beta", "v0.2.0", "v1.0.0"}
	for i := range ordered {
		for j := range ordered {
			got := Compare(ordered[i], ordered[j])
			if (i < j) != (got < 0) || (i == j) != (got == 0) {
				t.Errorf("Compare(%s, %s) = %d", ordered[i], ordered[j], got)
			}
		}
	}
	if Compare("dev", "v0.0.1") >= 0 {
		t.Error("a build with no release version was not older than every release")
	}
}
