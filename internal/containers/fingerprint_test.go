package containers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

func TestIncusFingerprint(t *testing.T) {
	// As Incus computes it: the SHA-256 of the layer digests, concatenated.
	sum := sha256.Sum256([]byte("sha256:aaasha256:bbb"))
	if got := IncusFingerprint([]string{"sha256:aaa", "sha256:bbb"}); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("got %q", got)
	}
	if IncusFingerprint([]string{"a", "b"}) != IncusFingerprint([]string{"ab"}) {
		t.Error("layers are concatenated")
	}
}

func TestRegistryFingerprint(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	push := func(arch string) v1.Image {
		img, err := random.Image(64, 2)
		if err != nil {
			t.Fatal(err)
		}
		cfg, _ := img.ConfigFile()
		cfg.Architecture, cfg.OS = arch, "linux"
		img, err = mutate.ConfigFile(img, cfg)
		if err != nil {
			t.Fatal(err)
		}
		return img
	}
	amd, arm := push("amd64"), push("arm64")
	idx := mutate.AppendManifests(mutate.IndexMediaType(empty.Index, "application/vnd.oci.image.index.v1+json"),
		mutate.IndexAddendum{Add: amd, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}}},
		mutate.IndexAddendum{Add: arm, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "arm64"}}},
	)
	ref := host + "/library/app:1"
	parsed, err := name.ParseReference(ref, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(parsed, idx); err != nil {
		t.Fatal(err)
	}

	r := &Registry{}
	for arch, img := range map[string]v1.Image{"amd64": amd, "arm64": arm} {
		got, err := r.Fingerprint(context.Background(), ref, arch)
		if err != nil {
			t.Fatal(err)
		}
		layers, _ := img.Layers()
		var digests []string
		for _, l := range layers {
			d, _ := l.Digest()
			digests = append(digests, d.String())
		}
		if want := IncusFingerprint(digests); got != want {
			t.Errorf("%s: got %s, want %s", arch, got, want)
		}
	}
	if fp, _ := r.Fingerprint(context.Background(), ref, "amd64"); len(r.fingerprints) != 2 || fp == "" {
		t.Errorf("fingerprints not cached by manifest digest: %v", r.fingerprints)
	}
	if _, err := r.Fingerprint(context.Background(), host+"/library/missing:1", "amd64"); err == nil {
		t.Error("expected an error for a missing image")
	}
}
