package containers

import (
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// DigestResolver looks up the current registry digest of an image reference.
type DigestResolver interface {
	Digest(ctx context.Context, ref string) (string, error)
}

// Registry resolves digests with HEAD requests against the registry, which
// do not count against Docker Hub pull limits. Results are cached until
// Reset, so one check cycle asks the registry once per reference.
type Registry struct {
	mu    sync.Mutex
	cache map[string]registryResult
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

	digest, err := headDigest(ctx, ref)

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

func headDigest(ctx context.Context, ref string) (string, error) {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return "", err
	}
	desc, err := remote.Head(parsed, remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil {
		return "", err
	}
	return desc.Digest.String(), nil
}

// ImageUpdate is an image with a newer version in its registry.
type ImageUpdate struct {
	Image      string   `json:"image"`
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
		update := ImageUpdate{Image: img, Managed: true}
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
