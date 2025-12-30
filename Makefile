.PHONY: build test format install

build:
	go build -o gh-issue-sync ./cmd/gh-issue-sync

test:
	go test ./...

format:
	go fmt ./...

install: build
	@INSTALL_DIR=""; \
	if command -v gh-issue-sync >/dev/null 2>&1; then \
		INSTALL_DIR=$$(dirname $$(which gh-issue-sync)); \
	else \
		for dir in "$$HOME/.local/bin" "$$HOME/.bin" "$$HOME/bin"; do \
			if [ -d "$$dir" ] && echo "$$PATH" | tr ':' '\n' | grep -qx "$$dir"; then \
				INSTALL_DIR="$$dir"; \
				break; \
			fi; \
		done; \
	fi; \
	if [ -z "$$INSTALL_DIR" ]; then \
		echo "error: no suitable install directory found on PATH"; \
		echo "hint: create ~/.local/bin and add it to your PATH"; \
		exit 1; \
	fi; \
	cp gh-issue-sync "$$INSTALL_DIR/gh-issue-sync"; \
	echo "installed to $$INSTALL_DIR/gh-issue-sync"
