# Agents guide

trains is an Italian train delay tracker with notifications and a status
page, written in Go. The README describes how it works, its configuration
and deployment.

## Working here

- Develop on a branch from `master` and merge back into it.
- `make test` runs `go vet` and the tests; run it before pushing.
- `make build` builds `./trains`; `make package` builds the release
  archives into `dist/`.
- To release, bump `VERSION` on `master` (see "Releasing" in the README).
