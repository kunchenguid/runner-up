// Package release fetches and checks runner-up's own release archives, as the
// release workflow publishes them (.github/workflows/release.yml): one
// archive per platform, runner-up-<version>-<goos>-<goarch>.tar.gz, or .zip
// for Windows, holding the program alone; checksums.txt, each archive's
// SHA-256 as sha256sum prints it; and checksums.txt.sig, the release signing
// key's Ed25519 signature of checksums.txt.
//
// The coordinator trusts an archive whose SHA-256 the setup names. runner-up's
// Windows service, which runs as SYSTEM, trusts only an archive that
// checksums.txt lists and the release signing key signed (SigningKeys).
package release

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// DefaultURL is where GitHub serves runner-up's releases: an archive is at
// DefaultURL/<version>/<archive>.
const DefaultURL = "https://github.com/kunchenguid/runner-up/releases/download"

// Platforms are the platforms each release has an archive for, as goos-goarch.
var Platforms = []string{"darwin-arm64", "darwin-amd64", "linux-arm64", "linux-amd64", "windows-arm64", "windows-amd64"}

// Platform names goos/goarch as a release does.
func Platform(goos, goarch string) string { return goos + "-" + goarch }

// ArchiveName is the release archive of version for platform.
func ArchiveName(version, platform string) string {
	if strings.HasPrefix(platform, "windows-") {
		return "runner-up-" + version + "-" + platform + ".zip"
	}
	return "runner-up-" + version + "-" + platform + ".tar.gz"
}

// ProgramName is the program's file name in platform's archive.
func ProgramName(platform string) string {
	if strings.HasPrefix(platform, "windows-") {
		return "runner-up.exe"
	}
	return "runner-up"
}

var versionPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$`)

// ValidVersion says whether v names a release, such as v0.1.11.
func ValidVersion(v string) bool { return versionPattern.MatchString(v) }

// Compare orders two release versions as semantic versions: negative when a
// is older than b, zero when they are the same release, positive when newer.
// A version that names no release is older than every release.
func Compare(a, b string) int {
	ma, mb := versionPattern.FindStringSubmatch(a), versionPattern.FindStringSubmatch(b)
	switch {
	case ma == nil && mb == nil:
		return strings.Compare(a, b)
	case ma == nil:
		return -1
	case mb == nil:
		return 1
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.ParseUint(ma[i], 10, 64)
		y, _ := strconv.ParseUint(mb[i], 10, 64)
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	// A pre-release comes before its release.
	pa, pb := strings.TrimPrefix(ma[4], "-"), strings.TrimPrefix(mb[4], "-")
	switch {
	case pa == pb:
		return 0
	case pa == "":
		return 1
	case pb == "":
		return -1
	}
	return comparePre(strings.Split(pa, "."), strings.Split(pb, "."))
}

func comparePre(a, b []string) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		x, errx := strconv.ParseUint(a[i], 10, 64)
		y, erry := strconv.ParseUint(b[i], 10, 64)
		switch {
		case errx == nil && erry == nil && x != y:
			if x < y {
				return -1
			}
			return 1
		case errx == nil && erry != nil:
			return -1
		case errx != nil && erry == nil:
			return 1
		case errx != nil && erry != nil && a[i] != b[i]:
			return strings.Compare(a[i], b[i])
		}
	}
	return len(a) - len(b)
}

// MaxArchive bounds an archive or the program in it, so a wrong address
// cannot fill the disk or memory.
const MaxArchive = 256 << 20

// Fetch reads url whole, at most limit bytes.
func Fetch(ctx context.Context, client *http.Client, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", url, err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", url, limit)
	}
	return b, nil
}

// SHA256 is b's SHA-256 in lowercase hex.
func SHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ValidSHA256 says whether s is a SHA-256 in lowercase hex.
func ValidSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

// Program is the program in platform's archive, which must hold it.
func Program(archive []byte, platform string) ([]byte, error) {
	name := ProgramName(platform)
	if strings.HasPrefix(platform, "windows-") {
		zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return nil, fmt.Errorf("the archive is not a zip file: %w", err)
		}
		for _, f := range zr.File {
			if f.Name != name {
				continue
			}
			if f.UncompressedSize64 > MaxArchive {
				return nil, fmt.Errorf("%s in the archive is too large", name)
			}
			r, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer r.Close()
			return readAtMost(r, name)
		}
		return nil, fmt.Errorf("the archive holds no %s", name)
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("the archive is not gzip: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("the archive holds no %s", name)
		}
		if err != nil {
			return nil, fmt.Errorf("the archive is not a tar file: %w", err)
		}
		if strings.TrimPrefix(h.Name, "./") == name && h.Typeflag == tar.TypeReg {
			return readAtMost(tr, name)
		}
	}
}

func readAtMost(r io.Reader, name string) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxArchive+1))
	if err != nil {
		return nil, fmt.Errorf("could not read %s from the archive: %w", name, err)
	}
	if len(b) > MaxArchive {
		return nil, fmt.Errorf("%s in the archive is too large", name)
	}
	return b, nil
}

// ParseChecksums reads checksums.txt: each line a SHA-256 in hex, two spaces
// (or a space and *, for a binary file) and a file name.
func ParseChecksums(b []byte) (map[string]string, error) {
	out := map[string]string{}
	for i, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		line = strings.TrimSuffix(line, "\r")
		sum, name, ok := strings.Cut(line, " ")
		name = strings.TrimPrefix(strings.TrimPrefix(name, " "), "*")
		sum = strings.ToLower(sum)
		if !ok || !ValidSHA256(sum) || name == "" || strings.ContainsAny(name, `/\ `) {
			return nil, fmt.Errorf("checksums line %d is not a SHA-256 and a file name", i+1)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("checksums list %s twice", name)
		}
		out[name] = sum
	}
	return out, nil
}

// ErrNoSigningKey: this runner-up was built with no release signing key, so it
// trusts no signature.
var ErrNoSigningKey = errors.New("this runner-up has no release signing key, so it accepts no signed release")

// VerifySignature checks that sig is an Ed25519 signature of checksums by one
// of keys.
func VerifySignature(checksums, sig []byte, keys []ed25519.PublicKey) error {
	if len(keys) == 0 {
		return ErrNoSigningKey
	}
	if len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("the signature is %d bytes, not an Ed25519 signature's %d", len(sig), ed25519.SignatureSize)
	}
	for _, k := range keys {
		if ed25519.Verify(k, checksums, sig) {
			return nil
		}
	}
	return errors.New("the release signing key did not sign these checksums")
}

// SigningKeys are the public halves of the release signing keys this
// runner-up trusts, parsed from signingKeys (key.go).
func SigningKeys() []ed25519.PublicKey {
	var out []ed25519.PublicKey
	for _, s := range strings.Fields(signingKeys) {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil || len(b) != ed25519.PublicKeySize {
			panic(fmt.Sprintf("release signing key %q is not a base64 Ed25519 public key", s))
		}
		out = append(out, ed25519.PublicKey(b))
	}
	return out
}

// Signed is a release archive with the signed checksums that vouch for it.
type Signed struct {
	// Name is the archive's file name, as checksums.txt lists it.
	Name      string
	Archive   []byte
	Checksums []byte
	Signature []byte
}

// archivePattern reads a release archive's name.
var archivePattern = regexp.MustCompile(`^runner-up-(v[0-9A-Za-z.-]+)-([a-z0-9]+-[a-z0-9]+)\.(tar\.gz|zip)$`)

// Verify checks a signed archive for platform with keys: the signature, that
// the checksums list the archive under its name, that the archive matches,
// and that the name is platform's archive of a release. It returns the
// archive's version.
func (s Signed) Verify(platform string, keys []ed25519.PublicKey) (string, error) {
	if err := VerifySignature(s.Checksums, s.Signature, keys); err != nil {
		return "", err
	}
	m := archivePattern.FindStringSubmatch(s.Name)
	if m == nil || !ValidVersion(m[1]) || ArchiveName(m[1], m[2]) != s.Name {
		return "", fmt.Errorf("%q is not a runner-up release archive", s.Name)
	}
	if m[2] != platform {
		return "", fmt.Errorf("%s is for %s, not %s", s.Name, m[2], platform)
	}
	sums, err := ParseChecksums(s.Checksums)
	if err != nil {
		return "", err
	}
	want, ok := sums[s.Name]
	if !ok {
		return "", fmt.Errorf("the signed checksums do not list %s", s.Name)
	}
	if got := SHA256(s.Archive); got != want {
		return "", fmt.Errorf("%s's SHA-256 is %s, not the %s the signed checksums list", s.Name, got, want)
	}
	return m[1], nil
}
