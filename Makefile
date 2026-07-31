# go-mp3packer — Makefile
#
# Targets:
#   init-hooks Enable git hooks for this repo

.PHONY: init-hooks
init-hooks:
	git config core.hooksPath .githooks
