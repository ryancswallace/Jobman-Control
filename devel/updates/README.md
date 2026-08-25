# Repository update scripts

`make update` runs executable `*.sh` files here in lexical order and exports
`GO_VERS` from `go.version`. Scripts must be POSIX `sh`, deterministic,
non-interactive, idempotent, and safe to rerun.

`go-vers.sh` synchronizes the selected Go version across the module, linter,
containers, devcontainer, workflows, and documentation. Container base-image
tags are also digest-pinned; update the matching digest whenever a tag changes.
