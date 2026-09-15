package spec

// LocalFiles returns every build-context path this install block reads, in a
// stable order and without duplicates.
//
// This is deliberately the ONE place that answers the question. Two consumers
// need it — the derived .dockerignore allowlist, and the CLI's native-build
// dependency hash — and while each kept its own copy they drifted: install.uv's
// pyproject.toml and uv.lock reached the allowlist but not the hash. Because
// neither the generated Dockerfile nor the lockfile varies with a uv lock's
// contents, editing a dependency then changed nothing the hash could see, and
// the native fast path shipped an image built from the previous dependency set.
//
// An ecosystem that resolves purely from the network (apt, apk, cmake) reads
// nothing here and contributes nothing.
func (i *Install) LocalFiles() []string {
	if i == nil {
		return nil
	}
	var paths []string
	seen := map[string]bool{}
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	for _, p := range i.Pip {
		add(p.Requirements)
	}
	if i.Npm != nil {
		add(NpmManifest)
		add(NpmLockfile(i.Npm.Manager))
	}
	if i.Uv != nil {
		for _, p := range UvLocalFiles {
			add(p)
		}
	}
	return paths
}
