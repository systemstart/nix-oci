// Package oci implements a minimal, deterministic OCI image layout writer.
package oci

import (
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// The OCI document types are re-exported from opencontainers/image-spec rather
// than hand-rolled. Vendoring the upstream definitions is what defends against
// the history <-> rootfs.diff_ids alignment trap (and similar spec subtleties)
// that hand-maintained structs are prone to. Keeping the aliases confines the
// dependency to this file: the rest of the writer speaks in oci.Manifest,
// oci.Config, etc., so a future source swap touches only here.
type (
	Descriptor  = v1.Descriptor
	Platform    = v1.Platform
	Index       = v1.Index
	Manifest    = v1.Manifest
	Config      = v1.Image
	ImageConfig = v1.ImageConfig
	RootFS      = v1.RootFS
	History     = v1.History
	ImageLayout = v1.ImageLayout
	Versioned   = specs.Versioned
)

// Media types and well-known strings, likewise sourced from image-spec so they
// cannot drift. Everything we emit is OCI; no Docker media types appear in the
// output.
const (
	MediaTypeIndex     = v1.MediaTypeImageIndex
	MediaTypeManifest  = v1.MediaTypeImageManifest
	MediaTypeConfig    = v1.MediaTypeImageConfig
	MediaTypeLayerGzip = v1.MediaTypeImageLayerGzip

	// The layout marker file and its only currently valid version.
	ImageLayoutFile    = v1.ImageLayoutFile
	ImageLayoutVersion = v1.ImageLayoutVersion

	// AnnotationRefName is how a layout records a tag; skopeo and crane use it
	// to select a manifest out of index.json.
	AnnotationRefName = v1.AnnotationRefName
)
