package image

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"sync"

	log "github.com/sirupsen/logrus"

	"github.com/akuity/kargo/internal/logging"
)

// newestBuildSelector implements the Selector interface for
// SelectionStrategyNewestBuild.
type newestBuildSelector struct {
	repoClient     *repositoryClient
	allowRegex     *regexp.Regexp
	ignore         []string
	platform       *platformConstraint
	discoveryLimit int
}

// newNewestBuildSelector returns an implementation of the Selector interface
// for SelectionStrategyNewestBuild.
func newNewestBuildSelector(
	repoClient *repositoryClient,
	allowRegex *regexp.Regexp,
	ignore []string,
	platform *platformConstraint,
	discoveryLimit int,
) Selector {
	return &newestBuildSelector{
		repoClient:     repoClient,
		allowRegex:     allowRegex,
		ignore:         ignore,
		platform:       platform,
		discoveryLimit: discoveryLimit,
	}
}

// Select implements the Selector interface.
func (n *newestBuildSelector) Select(ctx context.Context) ([]Image, error) {
	logger := logging.LoggerFromContext(ctx).WithFields(log.Fields{
		"registry":            n.repoClient.registry.name,
		"image":               n.repoClient.repoURL,
		"selectionStrategy":   SelectionStrategyNewestBuild,
		"platformConstrained": n.platform != nil,
		"discoveryLimit":      n.discoveryLimit,
	})
	logger.Trace("discovering images")

	ctx = logging.ContextWithLogger(ctx, logger)

	images, err := n.selectImages(ctx)
	if err != nil || len(images) == 0 {
		return nil, err
	}

	limit := n.discoveryLimit
	if limit == 0 || limit > len(images) {
		limit = len(images)
	}

	if n.platform == nil {
		for _, image := range images[:limit] {
			logger.WithFields(log.Fields{
				"tag":    image.Tag,
				"digest": image.Digest,
			}).Trace("discovered image")
		}
		logger.Tracef("discovered %d images", limit)
		return images[:limit], nil
	}

	// TODO(hidde): this could be more efficient, as we are fetching the image
	// _again_ to check if it matches the platform constraint (although we do
	// cache it indefinitely). We should consider refactoring this to avoid
	// fetching the image twice.
	discoveredImages := make([]Image, 0, limit)
	for _, image := range images {
		if len(discoveredImages) >= limit {
			break
		}

		discoveredImage, err := n.repoClient.getImageByDigest(ctx, image.Digest, n.platform)
		if err != nil {
			return nil, fmt.Errorf("error retrieving image with digest %q: %w", image.Digest, err)
		}

		if discoveredImage == nil {
			logger.Tracef(
				"image with digest %q was found, but did not match platform constraint",
				image.Digest,
			)
			continue
		}

		logger.WithFields(log.Fields{
			"tag":    image.Tag,
			"digest": image.Digest,
		}).Trace("discovered image")

		discoveredImage.Tag = image.Tag
		discoveredImages = append(discoveredImages, *discoveredImage)
	}

	if len(discoveredImages) == 0 {
		logger.Trace("no images matched platform constraint")
		return nil, nil
	}

	logger.Tracef("discovered %d images", len(discoveredImages))
	return discoveredImages, nil
}

func (n *newestBuildSelector) selectImages(ctx context.Context) ([]Image, error) {
	logger := logging.LoggerFromContext(ctx)

	tags, err := n.repoClient.getTags(ctx)
	if err != nil {
		return nil, fmt.Errorf("error listing tags: %w", err)
	}
	if len(tags) == 0 {
		logger.Trace("found no tags")
		return nil, nil
	}
	logger.Trace("got all tags")

	if n.allowRegex != nil || len(n.ignore) > 0 {
		matchedTags := make([]string, 0, len(tags))
		for _, tag := range tags {
			if allowsTag(tag, n.allowRegex) && !ignoresTag(tag, n.ignore) {
				matchedTags = append(matchedTags, tag)
			}
		}
		if len(matchedTags) == 0 {
			logger.Trace("no tags matched criteria")
			return nil, nil
		}
		tags = matchedTags
	}
	logger.Tracef("%d tags matched criteria", len(tags))

	logger.Trace("retrieving images for all tags that matched criteria")
	images, err := n.getImagesByTags(ctx, tags)
	if err != nil {
		return nil, fmt.Errorf("error retrieving images for all matched tags: %w", err)
	}
	if len(images) == 0 {
		// This shouldn't happen
		return nil, nil
	}

	logger.Trace("sorting images by date")
	sortImagesByDate(images)
	return images, nil
}

// getImagesByTags returns Image structs for the provided tags. Since the number
// of tags can often be large, this is done concurrently, with a package-level
// semaphore being used to limit the total number of running goroutines. The
// underlying repository client also uses built-in registry-level rate-limiting
// to avoid overwhelming any registry.
func (n *newestBuildSelector) getImagesByTags(
	ctx context.Context,
	tags []string,
) ([]Image, error) {
	// We'll cancel this context at the first error we encounter so that other
	// goroutines can stop early.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup

	// This channel is for collecting results
	imageCh := make(chan Image, len(tags))
	// This buffered channel has room for one error
	errCh := make(chan error, 1)

	for _, tag := range tags {
		if err := metaSem.Acquire(ctx, 1); err != nil {
			return nil, fmt.Errorf(
				"error acquiring semaphore for retrieval of image with tag %q: %w",
				tag,
				err,
			)
		}
		wg.Add(1)
		go func(tag string) {
			defer wg.Done()
			defer metaSem.Release(1)
			image, err := n.repoClient.getImageByTag(ctx, tag, nil)
			if err != nil {
				// Report the error right away or not at all. errCh is a buffered
				// channel with room for one error, so if we can't send the error
				// right away, we know that another goroutine has already sent one.
				select {
				case errCh <- err:
					cancel() // Stop all other goroutines
				default:
				}
				return
			}
			if image == nil {
				// This shouldn't happen
				return
			}
			// imageCh is buffered and sized appropriately, so this will never block.
			imageCh <- *image
		}(tag)
	}
	wg.Wait()
	// Check for and handle errors
	select {
	case err := <-errCh:
		return nil, err
	default:
	}
	close(imageCh)
	if len(imageCh) == 0 {
		return nil, nil
	}
	// Unpack the channel into a slice
	images := make([]Image, len(imageCh))
	for i := range images {
		// This will never block because we know that the channel is closed,
		// we know exactly how many items are in it, and we don't loop past that
		// number.
		images[i] = <-imageCh
	}
	return images, nil
}

// sortImagesByDate sorts the provided images in place, in chronologically
// descending order, breaking ties lexically by tag.
func sortImagesByDate(images []Image) {
	sort.Slice(images, func(i, j int) bool {
		if images[i].CreatedAt.Equal(*images[j].CreatedAt) {
			// If there's a tie on the date, break the tie lexically by name
			return images[i].Tag > images[j].Tag
		}
		return images[i].CreatedAt.After(*images[j].CreatedAt)
	})
}
