package oci

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// layerPopularity returns per-path popularity from the closure graph file, or
// nil when no graph was supplied (assignLayers then falls back to name order).
func layerPopularity(closureGraph string) (map[string]int, error) {
	if closureGraph == "" {
		return nil, nil
	}

	refs, err := readClosureGraph(closureGraph)
	if err != nil {
		return nil, err
	}

	return popularityOf(refs), nil
}

// readClosureGraph parses closureInfo's registration file at path.
func readClosureGraph(path string) (map[string][]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open closure graph: %w", err)
	}
	defer func() { _ = f.Close() }()

	return parseClosureGraph(f)
}

// parseClosureGraph reads the registration format closureInfo emits and returns
// each store path's direct references (self-references dropped). Each record is
// six-plus lines: path, narHash, narSize, deriver, a reference count, then that
// many reference lines. This is the same format `nix-store --load-db` consumes.
func parseClosureGraph(r io.Reader) (map[string][]string, error) {
	sc := bufio.NewScanner(r)

	next := func() (string, bool) {
		if sc.Scan() {
			return sc.Text(), true
		}

		return "", false
	}

	refs := make(map[string][]string)

	for {
		path, references, done, err := parseGraphRecord(next)
		if err != nil {
			return nil, err
		}

		if done {
			break
		}

		refs[path] = references
	}

	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("closure graph: %w", err)
	}

	return refs, nil
}

// parseGraphRecord reads one registration record. done is true at a clean EOF
// between records.
func parseGraphRecord(next func() (string, bool)) (path string, refs []string, done bool, err error) {
	path, ok := next()
	if !ok {
		return "", nil, true, nil
	}

	// Skip narHash, narSize, deriver.
	for range 3 {
		if _, ok := next(); !ok {
			return "", nil, false, fmt.Errorf("closure graph: truncated record for %q", path)
		}
	}

	countLine, ok := next()
	if !ok {
		return "", nil, false, fmt.Errorf("closure graph: missing reference count for %q", path)
	}

	count, err := strconv.Atoi(strings.TrimSpace(countLine))
	if err != nil {
		return "", nil, false, fmt.Errorf("closure graph: bad reference count %q for %q: %w", countLine, path, err)
	}

	refs = make([]string, 0, count)

	for range count {
		ref, ok := next()
		if !ok {
			return "", nil, false, fmt.Errorf("closure graph: truncated references for %q", path)
		}

		if ref != path {
			refs = append(refs, ref)
		}
	}

	return path, refs, false, nil
}

// popularityOf ranks each path by how many closure members transitively depend
// on it. Base dependencies (glibc and friends) score highest, so the layer
// assignment can isolate them into their own widely-shared layers. Computed by
// counting, per path, the reverse-reachable set in the reference graph.
func popularityOf(refs map[string][]string) map[string]int {
	reverse := make(map[string][]string, len(refs))
	for p, rs := range refs {
		for _, r := range rs {
			reverse[r] = append(reverse[r], p)
		}
	}

	popularity := make(map[string]int, len(refs))

	for p := range refs {
		seen := make(map[string]bool)
		stack := []string{p}

		for len(stack) > 0 {
			n := stack[len(stack)-1]
			stack = stack[:len(stack)-1]

			for _, dependent := range reverse[n] {
				if dependent != p && !seen[dependent] {
					seen[dependent] = true
					stack = append(stack, dependent)
				}
			}
		}

		popularity[p] = len(seen)
	}

	return popularity
}
