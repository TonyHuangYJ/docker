package images

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/containerd/containerd/v2/core/content"
	c8dimages "github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/leases"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/containerd/platforms"
	"github.com/distribution/reference"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/image"
	"github.com/moby/moby/api/types/backend"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

// ErrImageDoesNotExist is error returned when no image can be found for a reference.
type ErrImageDoesNotExist struct {
	Ref reference.Reference
}

func (e ErrImageDoesNotExist) Error() string {
	ref := e.Ref
	if named, ok := ref.(reference.Named); ok {
		ref = reference.TagNameOnly(named)
	}
	return fmt.Sprintf("No such image: %s", reference.FamiliarString(ref))
}

// NotFound implements the NotFound interface
func (e ErrImageDoesNotExist) NotFound() {}

type manifestList struct {
	Manifests []ocispec.Descriptor `json:"manifests"`
}

type manifest struct {
	Config ocispec.Descriptor `json:"config"`
}

func (i *ImageService) manifestMatchesPlatform(ctx context.Context, img *image.Image, platform ocispec.Platform) (bool, error) {
	ls, err := i.leases.ListResources(ctx, leases.Lease{ID: imageKey(img.ID().String())})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return false, nil
		}
		log.G(ctx).WithFields(log.Fields{
			"error":           err,
			"image":           img.ID,
			"desiredPlatform": platforms.FormatAll(platform),
		}).Error("Error looking up image leases")
		return false, err
	}

	// Note we are comparing against manifest lists here, which we expect to always have a CPU variant set (where applicable).
	// So there is no need for the fallback matcher here.
	comparer := platforms.Only(platform)

	var (
		ml manifestList
		m  manifest
	)

	makeRdr := func(ra content.ReaderAt) io.Reader {
		return io.LimitReader(io.NewSectionReader(ra, 0, ra.Size()), 1e6)
	}

	for _, r := range ls {
		logger := log.G(ctx).WithFields(log.Fields{
			"image":           img.ID,
			"desiredPlatform": platforms.FormatAll(platform),
			"resourceID":      r.ID,
			"resourceType":    r.Type,
		})
		logger.Debug("Checking lease resource for platform match")
		if r.Type != "content" {
			continue
		}

		ra, err := i.content.ReaderAt(ctx, ocispec.Descriptor{Digest: digest.Digest(r.ID)})
		if err != nil {
			if cerrdefs.IsNotFound(err) {
				continue
			}
			logger.WithError(err).Error("Error looking up referenced manifest list for image")
			continue
		}

		data, err := io.ReadAll(makeRdr(ra))
		ra.Close()

		if err != nil {
			logger.WithError(err).Error("Error reading manifest list for image")
			continue
		}

		ml.Manifests = nil

		if err := json.Unmarshal(data, &ml); err != nil {
			logger.WithError(err).Error("Error unmarshalling content")
			continue
		}

		for _, md := range ml.Manifests {
			switch md.MediaType {
			case ocispec.MediaTypeImageManifest, c8dimages.MediaTypeDockerSchema2Manifest:
			default:
				continue
			}

			p := ocispec.Platform{
				Architecture: md.Platform.Architecture,
				OS:           md.Platform.OS,
				Variant:      md.Platform.Variant,
			}
			if !comparer.Match(p) {
				logger.WithField("otherPlatform", platforms.FormatAll(p)).Debug("Manifest is not a match")
				continue
			}

			// Here we have a platform match for the referenced manifest, let's make sure the manifest is actually for the image config we are using.

			ra, err := i.content.ReaderAt(ctx, ocispec.Descriptor{Digest: md.Digest})
			if err != nil {
				logger.WithField("otherDigest", md.Digest).WithError(err).Error("Could not get reader for manifest")
				continue
			}

			data, err := io.ReadAll(makeRdr(ra))
			ra.Close()
			if err != nil {
				logger.WithError(err).Error("Error reading manifest for image")
				continue
			}

			if err := json.Unmarshal(data, &m); err != nil {
				logger.WithError(err).Error("Error desserializing manifest")
				continue
			}

			if m.Config.Digest == img.ID().Digest() {
				logger.WithField("manifestDigest", md.Digest).Debug("Found matching manifest for image")
				return true, nil
			}

			logger.WithField("otherDigest", md.Digest).Debug("Skipping non-matching manifest")
		}
	}

	return false, nil
}

// GetImage returns an image corresponding to the image referred to by refOrID.
func (i *ImageService) GetImage(ctx context.Context, refOrID string, options backend.GetImageOpts) (retImg *image.Image, retErr error) {
	defer func() {
		if retErr != nil || retImg == nil || options.Platform == nil {
			return
		}

		imgPlat := ocispec.Platform{
			OS:           retImg.OS,
			Architecture: retImg.Architecture,
			Variant:      retImg.Variant,
		}
		p := *options.Platform
		// Note that `platforms.Only` will fuzzy match this for us
		// For example: an armv6 image will run just fine on an armv7 CPU, without emulation or anything.
		if OnlyPlatformWithFallback(p).Match(imgPlat) {
			return
		}
		// In some cases the image config can actually be wrong (e.g. classic `docker build` may not handle `--platform` correctly)
		// So we'll look up the manifest list that corresponds to this image to check if at least the manifest list says it is the correct image.
		var matches bool
		matches, retErr = i.manifestMatchesPlatform(ctx, retImg, p)
		if matches || retErr != nil {
			return
		}

		// This allows us to tell clients that we don't have the image they asked for
		// Where this gets hairy is the image store does not currently support multi-arch images, e.g.:
		//   An image `foo` may have a multi-arch manifest, but the image store only fetches the image for a specific platform
		//   The image store does not store the manifest list and image tags are assigned to architecture specific images.
		//   So we can have a `foo` image that is amd64 but the user requested armv7. If the user looks at the list of images.
		//   This may be confusing.
		//   The alternative to this is to return an errdefs.Conflict error with a helpful message, but clients will not be
		//   able to automatically tell what causes the conflict.
		imgName := refOrID
		if ref, err := reference.ParseNamed(refOrID); err == nil {
			imgName = reference.FamiliarString(ref)
		}
		retErr = errdefs.NotFound(errors.Errorf("image with reference %s was found but its platform (%s) does not match the specified platform (%s)", imgName, platforms.FormatAll(imgPlat), platforms.FormatAll(p)))
	}()
	ref, err := reference.ParseAnyReference(refOrID)
	if err != nil {
		return nil, errdefs.InvalidParameter(err)
	}
	namedRef, ok := ref.(reference.Named)
	if !ok {
		digested, ok := ref.(reference.Digested)
		if !ok {
			return nil, ErrImageDoesNotExist{Ref: ref}
		}
		if img, err := i.imageStore.Get(image.ID(digested.Digest())); err == nil {
			return img, nil
		}
		return nil, ErrImageDoesNotExist{Ref: ref}
	}

	if dgst, err := i.referenceStore.Get(namedRef); err == nil {
		// Search the image stores to get the operating system, defaulting to host OS.
		if img, err := i.imageStore.Get(image.ID(dgst)); err == nil {
			return img, nil
		}
	}

	// Search based on ID
	if id, err := i.imageStore.Search(refOrID); err == nil {
		img, err := i.imageStore.Get(id)
		if err != nil {
			return nil, ErrImageDoesNotExist{Ref: ref}
		}
		return img, nil
	}

	return nil, ErrImageDoesNotExist{Ref: ref}
}

// OnlyPlatformWithFallback uses `platforms.Only` with a fallback to handle the case where the platform
// being matched does not have a CPU variant.
//
// The reason for this is that CPU variant is not even if the official image config spec as of this writing.
// See: https://github.com/opencontainers/image-spec/pull/809
// Since Docker tends to compare platforms from the image config, we need to handle this case.
func OnlyPlatformWithFallback(p ocispec.Platform) platforms.Matcher {
	return &onlyFallbackMatcher{only: platforms.Only(p), p: platforms.Normalize(p)}
}

type onlyFallbackMatcher struct {
	only platforms.Matcher
	p    ocispec.Platform
}

func (m *onlyFallbackMatcher) Match(other ocispec.Platform) bool {
	if m.only.Match(other) {
		// It matches, no reason to fallback
		return true
	}
	if other.Variant != "" {
		// If there is a variant then this fallback does not apply, and there is no match
		return false
	}

	// note that platforms.Normalize adds a default variant... which is the
	// whole problem with [platforms.Only], so we can't match on that.
	otherN := platforms.Normalize(other)
	return m.p.OS == otherN.OS && m.p.Architecture == otherN.Architecture
}
