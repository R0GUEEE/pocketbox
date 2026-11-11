// Package buildkitclient is the type namespace container images are described
// in: Dockerfile instructions, parsed and lowered into image layers.
//
// Only the instruction set the engine implements lives here. Anything that
// needs a builder daemon (multi-stage COPY --from, secret mounts, BuildKit's
// LLB cache) is rejected with a precise error instead of being approximated
// silently.
package buildkitclient
