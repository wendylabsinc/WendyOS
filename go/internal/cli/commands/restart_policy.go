package commands

// Explicit flags override the project's default, including on the unchanged
// deployment path. Resolve this before hashing desired container settings.
func (opts runOptions) withConfiguredRestartPolicy(policy string) runOptions {
	if opts.noRestart || opts.restartOnFailure || opts.restartUnlessStopped {
		return opts
	}
	switch policy {
	case "no":
		opts.noRestart = true
	case "on-failure":
		opts.restartOnFailure = true
	case "unless-stopped":
		opts.restartUnlessStopped = true
	}
	return opts
}
