# Fixes path issues for various commands
ifeq ($(OS),Windows_NT)
export PATH := /usr/bin;$(PATH)
endif

# COLORS
BLUE = \e[1;36m
GREEN = \e[1;32m
GRAY = \e[38;5;247m
RESET = \e[0m

# VARIABLES
# -------------------------------------
DOCS := docs
BUILD := build
IMAGE := wall-e
STATIC_FILES := $(shell find static -type f)
SRC_FILES := $(shell find src -type f)
DOCKER_DEPS := $(STATIC_FILES) Dockerfile $(BUILD)/pi-settings.json $(SRC_FILES) $(DOCS)/build/html/index.html
HOME := $(or $(HOME),$(USERPROFILE))

BOT := wall-e
TMUX_SESSION := default
AUTH_FILE := /opt/pi/auth.json
SETTINGS_FILE := /opt/pi/settings.json
WALLE_PORT ?= 6007
MD_FILES := $(shell find $(DOCS)/source -name "*.md")

# Optionally include environment variables
-include .env
export

# TARGETS
# -------------------------------------

.PHONY: help # Show help message
help:
	@echo -e '$(BLUE)wall-e$(RESET) Makefile$(GRAY)'
	@echo "List of Targets:"
	@cat $(MAKEFILE_LIST) | grep -E '^.PHONY: [a-zA-Z0-9_-]+ .*# .*$$' \
		| sed -E 's/.PHONY: ([^ ]+) .*# (.*)/  \1\t\2/' \
		| expand -t 20 \
		| sort

.PHONY: docker # Build and run container
docker: $(BUILD)/docker-stamp $(BUILD)/auth.json $(BUILD)/pi-settings.json
	-@docker rm -f $(IMAGE) 2>/dev/null
	# -e CLOUDFLARE_TOKEN
	@docker run -d \
		--name $(IMAGE) \
		-e TZ \
		-e WALLE_TOKEN \
		-e WALLE_PORT \
		-e WALLE_TELEGRAM_TOKEN \
		-e WALLE_TELEGRAM_ALLOWED_CHATS \
		-e OPENAI_API_KEY \
		-e OPENROUTER_API_KEY \
		-e BRAVE_API_KEY \
		-v "./$(BUILD)/auth.json:$(AUTH_FILE)" \
		-v "./$(BUILD)/pi-settings.json:$(SETTINGS_FILE)" \
		-v walle--home:/home/$(BOT) \
		-p $(WALLE_PORT):$(WALLE_PORT) \
		-p 6080:80 \
		$(IMAGE)

.PHONY: attach # Attach to container
attach:
	@docker exec -it -u wall-e $(IMAGE) tmux new-session -A -s $(TMUX_SESSION)


.PHONY: test # Run tests, optionally with -race via RACE=1
test:
	@cd src && go vet ./... && \
	if [ "$$RACE" = "1" ]; then \
		CC=x86_64-w64-mingw32-gcc go test -race -count=1 ./...; \
	else \
		go test -count=1 ./...; \
	fi

.PHONY: clean # Clean build artifacts
clean:
	@echo 'Cleaning up...'
	@rm -rf $(BUILD) .gotmp/ .ruff_cache/
	@sh -c 'find . -regex "^.*\(__pycache__\|\.py[co]\)$$ " -delete'
	@echo 'Done!'

.PHONY: debug # Show debug information
debug:
	@echo "HOME: $(HOME)"

# RECIPES
# -------------------------------------
$(BUILD)/docker-stamp: $(DOCKER_DEPS)
	@mkdir -p $(BUILD)
	@docker build --progress=plain -t $(IMAGE) . 2>&1 | tee $@

$(BUILD)/auth.json: scripts/codex-oauth.py
	@mkdir -p $(BUILD)
	@uv run scripts/codex-oauth.py > $@

$(BUILD)/pi-settings.json: $(HOME)/.pi/agent/settings.json
	@mkdir -p $(BUILD)
	@cat ~/.pi/agent/settings.json | jq '{ defaultProvider, defaultModel }' > $@

$(DOCS)/build/html/index.html: $(MD_FILES)
	@cd $(DOCS) && make clean html
