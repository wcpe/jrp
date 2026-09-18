TASK ?= task

.PHONY: bootstrap workspace-sync fmt lint typecheck test build clean

bootstrap workspace-sync fmt lint typecheck test build clean:
	$(TASK) $@
