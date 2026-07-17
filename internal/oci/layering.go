package oci

import "sort"

// assignLayers partitions store paths into ordered layer groups.
//
// The strategy is one store path per layer, up to maxLayers, so that a path
// shared between two images produces a byte-identical layer blob in both and the
// registry stores it once. When the closure has more paths than the cap allows,
// the overflow shares the final layer -- some dedup is lost for those paths, but
// the layer count stays bounded (overlayfs degrades past ~125 layers).
//
// Paths are ordered most-shareable first (by popularity, when a reference graph
// is available; otherwise by name). This decides which paths keep dedicated
// layers when the closure overflows the cap -- the widely-depended-on base paths
// (glibc, bash) stay isolated, and the image-specific tail is what gets lumped.
// The order is a pure function of the closure, so every layer digest is
// reproducible.
//
// maxLayers <= 1 collapses everything into a single layer (the M0/M1 behaviour).
func assignLayers(roots []string, maxLayers int, popularity map[string]int) [][]string {
	ordered := orderRoots(roots, popularity)

	if maxLayers <= 1 || len(ordered) <= 1 {
		return [][]string{ordered}
	}

	if len(ordered) <= maxLayers {
		groups := make([][]string, len(ordered))
		for i, r := range ordered {
			groups[i] = []string{r}
		}

		return groups
	}

	// More paths than layers: the first maxLayers-1 (most shareable) each get a
	// dedicated layer; the rest lump into the last.
	groups := make([][]string, 0, maxLayers)
	for i := range maxLayers - 1 {
		groups = append(groups, []string{ordered[i]})
	}

	return append(groups, ordered[maxLayers-1:])
}

// orderRoots returns roots most-shareable first: descending popularity, then by
// name for stability. With no popularity data it is plain name order (which is
// what keeps the store-path-per-layer output reproducible either way).
func orderRoots(roots []string, popularity map[string]int) []string {
	ordered := append([]string(nil), roots...)

	sort.SliceStable(ordered, func(i, j int) bool {
		if popularity != nil {
			if pi, pj := popularity[ordered[i]], popularity[ordered[j]]; pi != pj {
				return pi > pj
			}
		}

		return ordered[i] < ordered[j]
	})

	return ordered
}
