# This tool operates on files that live in the caller's tree, not in this
# repo. Their locations come from the environment so nothing here is tied to
# any particular consumer's layout:
#
#   API_GEN       path to the oapi-codegen output to read enum names from
#                 (e.g. export API_GEN=/path/to/server.gen.go)
#   API_SPEC      path to the OpenAPI spec to canonicalize
#                 (e.g. export API_SPEC=/path/to/api-spec.yaml)
#   API_SPEC_OUT  where `make run` writes the fixed spec. Defaults to
#                 API_SPEC with a "-fixed" suffix; set it equal to API_SPEC
#                 to rewrite the spec in place.
#
# `all` and `clean` need none of these; every other target requires
# API_GEN and API_SPEC.

API_SPEC_OUT ?= $(API_SPEC:.yaml=-fixed.yaml)

require = @[ -n "$($1)" ] || { echo "error: $1 is not set - see the comment at the top of this Makefile" >&2; exit 1; }

all:
	go mod tidy
	go vet
	staticcheck
	go build -o fix-api-spec

run: all
	$(call require,API_GEN)
	$(call require,API_SPEC)
	@[ "$(API_SPEC_OUT)" = "$(API_SPEC)" ] || rm -f "$(API_SPEC_OUT)"
	./fix-api-spec -gen "$(API_GEN)" -spec "$(API_SPEC)" -out "$(API_SPEC_OUT)"

# Ask the binary whether the spec is already canonical, writing nothing.
# Intended for the consuming build to gate `go generate` on a spec that's
# still in its canonical x-enum-varnames + named-enums form. Exit non-zero
# means the spec needs to be fixed (run `make run` in this directory).
check: all
	$(call require,API_GEN)
	$(call require,API_SPEC)
	./fix-api-spec -check -gen "$(API_GEN)" -spec "$(API_SPEC)"

clean:
	rm -rf fix-api-spec
	$(if $(filter-out $(API_SPEC),$(API_SPEC_OUT)),rm -f "$(API_SPEC_OUT)",@:)

.PHONY: all run check clean
