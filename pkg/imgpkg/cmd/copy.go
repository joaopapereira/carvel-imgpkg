// Copyright 2024 The Carvel Authors.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"

	"carvel.dev/imgpkg/pkg/imgpkg/bundle"
	ctlimgset "carvel.dev/imgpkg/pkg/imgpkg/imageset"
	"carvel.dev/imgpkg/pkg/imgpkg/internal/util"
	"carvel.dev/imgpkg/pkg/imgpkg/lockconfig"
	"carvel.dev/imgpkg/pkg/imgpkg/plainimage"
	"carvel.dev/imgpkg/pkg/imgpkg/registry"
	"carvel.dev/imgpkg/pkg/imgpkg/signature"
	v1 "carvel.dev/imgpkg/pkg/imgpkg/v1"
	"github.com/cppforlife/go-cli-ui/ui"
	"github.com/spf13/cobra"
)

type CopyOptions struct {
	ui ui.UI

	ImageFlags      ImageFlags
	BundleFlags     BundleFlags
	LockInputFlags  LockInputFlags
	LockOutputFlags LockOutputFlags
	TarFlags        TarFlags
	RegistryFlags   RegistryFlags
	SignatureFlags  SignatureFlags

	RepoDst string

	Concurrency             int
	IncludeNonDistributable bool
	UseRepoBasedTags        bool
}

// NewCopyOptions constructor for building a CopyOptions, holding values derived via flags
func NewCopyOptions(ui *ui.ConfUI) *CopyOptions {
	return &CopyOptions{ui: ui}
}

func NewCopyCmd(o *CopyOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "copy",
		Short: "Copy a bundle from one location to another",
		RunE:  func(_ *cobra.Command, _ []string) error { return o.Run() },
		Example: `
    # Copy bundle dkalinin/app1-bundle to local tarball at /Volumes/app1-bundle.tar
    imgpkg copy -b dkalinin/app1-bundle --to-tar /Volumes/app1-bundle.tar

    # Copy bundle dkalinin/app1-bundle to another registry (or repository)
    imgpkg copy -b dkalinin/app1-bundle --to-repo internal-registry/app1-bundle

    # Copy image dkalinin/app1-image to another registry (or repository)
    # ##########################################################################
    # NOTE: if not using ~/.docker.config for authn, use env vars as described  #
    # in https://carvel.dev/imgpkg/docs/v0.24.0/auth/#via-environment-variables #
    # ##########################################################################
    imgpkg copy -i dkalinin/app1-image --to-repo internal-registry/app1-image

    # Copy using image --repo-based-tags flag
    imgpkg copy -i registry.foo.bar/some/application/app \
                --to-repo other-reg.faz.baz/my-app --repo-based-tags

    # If the above source repo has a tag sha256:669e010b58baf5beb2836b253c1fd5768333f0d1dbcb834f7c07a4dc93f474be,
    # a new tag some-application-app-sha256-669e010b58baf5beb2836b253c1fd5768333f0d1dbcb834f7c07a4dc93f474be.imgpkg
    # will be created in the destination repo. Note that the part of the new tag preceeding '-sha256' will be truncated to
    # the last 49 charachters`,
	}

	o.ImageFlags.SetCopy(cmd)
	o.BundleFlags.SetCopy(cmd)
	o.LockInputFlags.Set(cmd)
	o.LockOutputFlags.SetOnCopy(cmd)
	o.TarFlags.Set(cmd)
	o.RegistryFlags.Set(cmd)
	o.SignatureFlags.Set(cmd)
	cmd.Flags().StringVar(&o.RepoDst, "to-repo", "", "Location to upload assets")
	cmd.Flags().IntVar(&o.Concurrency, "concurrency", 5, "Concurrency")
	cmd.Flags().BoolVar(&o.IncludeNonDistributable, "include-non-distributable-layers", false,
		"Include non-distributable layers when copying an image/bundle")
	cmd.Flags().BoolVar(&o.UseRepoBasedTags, "repo-based-tags", false,
		"Allow imgpkg to use repository-based tags for convenience")
	return cmd
}

func (c *CopyOptions) Run() error {
	if !c.hasOneSrc() {
		return fmt.Errorf("Expected either --lock, --bundle (-b), --image (-i), or --tar as a source")
	}
	if !c.hasOneDst() {
		return fmt.Errorf("Expected either --to-tar or --to-repo")
	}

	registryOpts := c.RegistryFlags.AsRegistryOpts()
	registryOpts.IncludeNonDistributableLayers = c.IncludeNonDistributable

	reg, err := registry.NewSimpleRegistry(registryOpts)
	if err != nil {
		return err
	}

	prefixedLogger := util.NewPrefixedLogger("copy | ", util.NewLogger(c.ui))
	levelLogger := util.NewUILevelLogger(util.LogWarn, prefixedLogger)
	imagesUploaderLogger := util.NewProgressBar(levelLogger, "done uploading images", "Error uploading images")

	var tagGen util.TagGenerator
	tagGen = util.DefaultTagGenerator{}
	if c.UseRepoBasedTags {
		tagGen = util.RepoBasedTagGenerator{}
	}

	imageSet := ctlimgset.NewImageSet(c.Concurrency, prefixedLogger, tagGen)
	tarImageSet := ctlimgset.NewTarImageSet(imageSet, c.Concurrency, prefixedLogger)

	var signatureRetriever v1.SignatureFetcher
	if c.SignatureFlags.CopyCosignSignatures {
		signatureRetriever = signature.NewSignatures(signature.NewCosign(reg), c.Concurrency)
	} else {
		signatureRetriever = signature.NewNoop()
	}

	opts := v1.CopyOpts{
		Logger:                  levelLogger,
		ImageSet:                imageSet,
		TarImageSet:             tarImageSet,
		Concurrency:             c.Concurrency,
		SignatureRetriever:      signatureRetriever,
		IncludeNonDistributable: c.IncludeNonDistributable,
		Resume:                  c.TarFlags.Resume,
	}

	switch {
	case c.TarFlags.IsDst():
		if c.TarFlags.IsSrc() {
			return fmt.Errorf("Cannot use tar source (--tar) with tar destination (--to-tar)")
		}
		if c.LockOutputFlags.LockFilePath != "" {
			return fmt.Errorf("Cannot output lock file with tar destination")
		}

		origin := v1.CopyOrigin{
			ImageRef:     c.ImageFlags.Image,
			BundleRef:    c.BundleFlags.Bundle,
			LockfilePath: c.LockInputFlags.LockFilePath,
		}
		ids, err := v1.CopyToTar(origin, c.TarFlags.TarDst, opts, registry.NewRegistryWithProgress(reg, imagesUploaderLogger))
		if err != nil {
			return err
		}
		informUserToUseTheNonDistributableFlagWithDescriptors(
			levelLogger, c.IncludeNonDistributable, getNonDistributableLayersFromImageDescriptors(ids))

		return nil

	case c.isRepoDst():
		if c.TarFlags.Resume {
			return fmt.Errorf("Flag --resume can only be used when copying to tar")
		}

		origin := v1.CopyOrigin{
			ImageRef:     c.ImageFlags.Image,
			BundleRef:    c.BundleFlags.Bundle,
			TarPath:      c.TarFlags.TarSrc,
			LockfilePath: c.LockInputFlags.LockFilePath,
		}

		processedImages, err := v1.CopyToRepository(origin, c.RepoDst, opts, reg)
		if err != nil {
			return err
		}

		informUserToUseTheNonDistributableFlagWithDescriptors(
			levelLogger, c.IncludeNonDistributable, processedImagesNonDistLayer(processedImages))

		return c.writeLockOutput(processedImages, reg)

	default:
		panic("Unreachable")
	}
}

func (c *CopyOptions) writeLockOutput(processedImages *ctlimgset.ProcessedImages, registry registry.Registry) error {
	if c.LockOutputFlags.LockFilePath == "" {
		return nil
	}

	processedImageRootBundle := c.findProcessedImageRootBundle(processedImages)

	if processedImageRootBundle != nil {
		// this is an optimization to avoid getting an image descriptor for an ImageIndex, since we know
		// it definetely will not be a bundle
		if processedImageRootBundle.ImageIndex != nil {
			panic(fmt.Errorf("Internal inconsistency: '%s' should be a bundle but it is not", processedImageRootBundle.DigestRef))
		}

		plainImg := plainimage.NewFetchedPlainImageWithTag(processedImageRootBundle.DigestRef, processedImageRootBundle.UnprocessedImageRef.Tag, processedImageRootBundle.Image)
		foundBundle := bundle.NewBundleFromPlainImage(plainImg, registry)
		ok, err := foundBundle.IsBundle()
		if err != nil {
			return fmt.Errorf("Check if '%s' is bundle: %s", processedImageRootBundle.DigestRef, err)
		}
		if !ok {
			panic(fmt.Errorf("Internal inconsistency: '%s' should be a bundle but it is not", processedImageRootBundle.DigestRef))
		}

		return c.writeBundleLockOutput(foundBundle)
	}

	// if the tarball was created with an older version (prior to assign a label to the root bundle) and it contains a bundle
	// then return an error to the user informing them to recreate the tarball, since we don't know which is the root bundle.
	err := c.informUserIfTarballNeedsToBeRecreated(processedImages, registry)
	if err != nil {
		return err
	}

	return c.writeImagesLockOutput(processedImages)
}

func (c *CopyOptions) findProcessedImageRootBundle(processedImages *ctlimgset.ProcessedImages) *ctlimgset.ProcessedImage {
	var bundleProcessedImage *ctlimgset.ProcessedImage

	for _, processedImage := range processedImages.All() {
		if v1.IsRootBundle(processedImage) {
			if bundleProcessedImage != nil {
				panic("Internal inconsistency: expected only 1 root bundle")
			}
			bundleProcessedImage = &ctlimgset.ProcessedImage{
				UnprocessedImageRef: processedImage.UnprocessedImageRef,
				DigestRef:           processedImage.DigestRef,
				Image:               processedImage.Image,
				ImageIndex:          processedImage.ImageIndex,
			}
		}
	}
	return bundleProcessedImage
}

func (c *CopyOptions) informUserIfTarballNeedsToBeRecreated(processedImages *ctlimgset.ProcessedImages, registry registry.Registry) error {
	for _, item := range processedImages.All() {
		// this is an optimization to avoid getting an image descriptor for an ImageIndex, since we know
		// it definetely will not be a bundle
		if item.ImageIndex != nil {
			continue
		}

		plainImg := plainimage.NewFetchedPlainImageWithTag(item.DigestRef, item.UnprocessedImageRef.Tag, item.Image)
		bundle := bundle.NewBundleFromPlainImage(plainImg, registry)

		ok, err := bundle.IsBundle()
		if err != nil {
			return fmt.Errorf("Check if '%s' is bundle: %s", item.DigestRef, err)
		}
		if ok {
			return fmt.Errorf("Unable to determine correct root bundle to use for lock-output. hint: if copying from a tarball, try re-generating the tarball")
		}
	}
	return nil
}

func (c *CopyOptions) isRepoDst() bool { return c.RepoDst != "" }

func (c *CopyOptions) hasOneDst() bool {
	repoSet := c.isRepoDst()
	tarSet := c.TarFlags.IsDst()
	return (repoSet || tarSet) && !(repoSet && tarSet)
}

func (c *CopyOptions) hasOneSrc() bool {
	var seen bool
	for _, ref := range []string{c.LockInputFlags.LockFilePath, c.TarFlags.TarSrc,
		c.BundleFlags.Bundle, c.ImageFlags.Image} {
		if ref != "" {
			if seen {
				return false
			}
			seen = true
		}
	}
	return seen
}

func (c *CopyOptions) writeImagesLockOutput(processedImages *ctlimgset.ProcessedImages) error {
	imagesLock := lockconfig.ImagesLock{
		LockVersion: lockconfig.LockVersion{
			APIVersion: lockconfig.ImagesLockAPIVersion,
			Kind:       lockconfig.ImagesLockKind,
		},
	}

	if c.LockInputFlags.LockFilePath != "" {
		var err error
		imagesLock, err = lockconfig.NewImagesLockFromPath(c.LockInputFlags.LockFilePath)
		if err != nil {
			return err
		}
		for i, image := range imagesLock.Images {
			img, found := processedImages.FindByURL(ctlimgset.UnprocessedImageRef{DigestRef: image.Image})
			if !found {
				return fmt.Errorf("Expected image '%s' to have been copied but was not", image.Image)
			}
			imagesLock.Images[i].Image = img.DigestRef
		}
	} else {
		for _, img := range processedImages.All() {
			imagesLock.Images = append(imagesLock.Images, lockconfig.ImageRef{
				Image: img.DigestRef,
			})
		}
	}

	return imagesLock.WriteToPath(c.LockOutputFlags.LockFilePath)
}

func (c *CopyOptions) writeBundleLockOutput(bundle *bundle.Bundle) error {
	bundleLock := lockconfig.BundleLock{
		LockVersion: lockconfig.LockVersion{
			APIVersion: lockconfig.BundleLockAPIVersion,
			Kind:       lockconfig.BundleLockKind,
		},
		Bundle: lockconfig.BundleRef{
			Image: bundle.DigestRef(),
			Tag:   bundle.Tag(),
		},
	}

	return bundleLock.WriteToPath(c.LockOutputFlags.LockFilePath)
}
