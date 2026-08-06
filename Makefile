# go-mp3packer — Makefile
#
# Targets:
#   init-hooks Enable git hooks for this repo
#   validate   Every lossless-validation script (needs ffmpeg, lame, mpg123)
#   validate-matrix / validate-random / validate-regression
#              The three individually — the header of scripts/lib.sh says what
#              they cover that `go test ./...` cannot

.PHONY: init-hooks
init-hooks:
	git config core.hooksPath .githooks

.PHONY: validate validate-matrix validate-random validate-regression
validate: validate-matrix validate-random validate-regression

validate-matrix:
	scripts/test_matrix.sh

validate-random:
	scripts/test_random.sh

validate-regression:
	scripts/test_regression.sh
