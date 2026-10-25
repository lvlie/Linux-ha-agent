package containers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// DigestResolver looks up the current registry digest of an image reference.
type DigestResolver interface {
	Digest(ctx context.Context, ref string) (string, error)
}

// FingerprintResolver computes the Incus fingerprint of the current version
// of an OCI image in its registry. arch is an OCI architecture, with its
// variant where there are several ("amd64", "arm/v7").
type FingerprintResolver interface {
	Fingerprint(ctx context.Context, ref, arch string) (string, error)
}

// Registry resolves digests with HEAD requests against the registry, which
// do not count against Docker Hub pull limits. Results are cached until
// Reset, so one check cycle asks the registry once per reference.
type Registry struct {
	mu    sync.Mutex
	cache map[string]registryResult
	// fingerprints by manifest digest and architecture; digests never
	// change, so this survives Reset.
	fingerprints map[string]string
	// Options are extra options for every registry request (tests).
	Options []remote.Option
}

type registryResult struct {
	digest string
	err    error
}

// Digest implements DigestResolver.
func (r *Registry) Digest(ctx context.Context, ref string) (string, error) {
	r.mu.Lock()
	if res, ok := r.cache[ref]; ok {
		r.mu.Unlock()
		return res.digest, res.err
	}
	r.mu.Unlock()

	digest, err := r.headDigest(ctx, ref)

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cache == nil {
		r.cache = map[string]registryResult{}
	}
	r.cache[ref] = registryResult{digest: digest, err: err}
	return digest, err
}

// Reset clears the cache.
func (r *Registry) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache = nil
}

func (r *Registry) options(ctx context.Context, extra ...remote.Option) []remote.Option {
	opts := []remote.Option{remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain)}
	return append(append(opts, r.Options...), extra...)
}

func (r *Registry) headDigest(ctx context.Context, ref string) (string, error) {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return "", err
	}
	desc, err := remote.Head(parsed, r.options(ctx)...)
	if err != nil {
		return "", err
	}
	return desc.Digest.String(), nil
}

// Fingerprint implements FingerprintResolver. Incus identifies an OCI image
// by the SHA-256 of its layer digests (as skopeo lists them for the
// architecture), so this is what a container launched from ref today would
// have in volatile.base_image. Only a HEAD request is made while the tag
// still points at a manifest seen before.
func (r *Registry) Fingerprint(ctx context.Context, ref, arch string) (string, error) {
	digest, err := r.Digest(ctx, ref)
	if err != nil {
		return "", err
	}
	key := digest + "|" + arch
	r.mu.Lock()
	fp, ok := r.fingerprints[key]
	r.mu.Unlock()
	if ok {
		return fp, nil
	}
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return "", err
	}
	img, err := remote.Image(parsed, r.options(ctx, remote.WithPlatform(platform(arch)))...)
	if err != nil {
		return "", err
	}
	manifest, err := img.Manifest()
	if err != nil {
		return "", err
	}
	layers := make([]string, 0, len(manifest.Layers))
	for _, l := range manifest.Layers {
		layers = append(layers, l.Digest.String())
	}
	fp = IncusFingerprint(layers)
	r.mu.Lock()
	if r.fingerprints == nil {
		r.fingerprints = map[string]string{}
	}
	r.fingerprints[key] = fp
	r.mu.Unlock()
	return fp, nil
}

// platform is the Linux platform of an architecture with an optional variant
// ("arm/v7"); empty is the agent's own.
func platform(arch string) v1.Platform {
	if arch == "" {
		arch = runtime.GOARCH
	}
	arch, variant, _ := strings.Cut(arch, "/")
	return v1.Platform{OS: "linux", Architecture: arch, Variant: variant}
}

// IncusFingerprint is how Incus fingerprints an OCI image: the SHA-256 of
// its layer digests written one after the other.
func IncusFingerprint(layers []string) string {
	h := sha256.New()
	for _, l := range layers {
		h.Write([]byte(l))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ImageUpdate is an image with a newer version in its registry.
type ImageUpdate struct {
	Image string `json:"image"`
	// Digest is the newer digest in the registry.
	Digest     string   `json:"digest"`
	Containers []string `json:"containers"`
	// Managed is true when the agent can update it (compose or podman
	// auto-update); other containers must be recreated by hand.
	Managed bool `json:"managed"`
}

// ImageCheck is the result of comparing local images with their registries.
type ImageCheck struct {
	Updates []ImageUpdate `json:"updates"`
	// Unknown counts images that could not be checked (local builds,
	// private registries without credentials, network errors).
	Unknown int `json:"unknown"`
}

// CheckImages compares the images used by containers with their registries.
func CheckImages(ctx context.Context, containers []Container, local map[string][]string, resolver DigestResolver) ImageCheck {
	byImage := map[string][]Container{}
	for _, c := range containers {
		if c.Image == "" || strings.Contains(c.Image, "@sha256:") {
			continue // pinned by digest: never "outdated"
		}
		byImage[c.Image] = append(byImage[c.Image], c)
	}
	images := make([]string, 0, len(byImage))
	for img := range byImage {
		images = append(images, img)
	}
	slices.Sort(images)

	var check ImageCheck
	for _, img := range images {
		users := byImage[img]
		localDigests := local[users[0].ImageID]
		if len(localDigests) == 0 {
			check.Unknown++
			continue
		}
		remoteDigest, err := resolver.Digest(ctx, img)
		if err != nil || remoteDigest == "" {
			check.Unknown++
			continue
		}
		if slices.Contains(localDigests, remoteDigest) {
			continue
		}
		update := ImageUpdate{Image: img, Digest: remoteDigest, Managed: true}
		for _, c := range users {
			update.Containers = append(update.Containers, c.Name)
			if c.Project == "" && !c.AutoUpdate {
				update.Managed = false
			}
		}
		check.Updates = append(check.Updates, update)
	}
	return check
}
