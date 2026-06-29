BUILD := build
IMAGE := wall-e
STATIC_FILES := $(shell find static -type f)
SRC_FILES := $(shell find src -type f)
DOCKER_DEPS := $(STATIC_FILES) Dockerfile $(BUILD)/pi-settings.json $(SRC_FILES)

BOT := wall-e
AUTH_FILE := /opt/pi/auth.json
SETTINGS_FILE := /opt/pi/settings.json

-include .env
export

# TARGETS
.PHONY: docker
docker: $(BUILD)/docker-stamp $(BUILD)/auth.json
	@docker run -d \
		--name $(IMAGE) \
		-e WALLE_TOKEN \
		-e WALLE_PORT \
		-e WALLE_TELEGRAM_TOKEN \
		-e WALLE_TELEGRAM_ALLOWED_CHATS \
		-e OPENAI_API_KEY \
		-e OPENROUTER_API_KEY \
		-v "./$(BUILD)/auth.json:$(AUTH_FILE)" \
		-v "./$(BUILD)/pi-settings.json:$(SETTINGS_FILE)" \
		-p $(WALLE_PORT):$(WALLE_PORT) \
		$(IMAGE)

# optionally with -race via RACE=1
.PHONY: test
test:
	@cd src && go vet ./... && \
	if [ "$$RACE" = "1" ]; then \
		CC=x86_64-w64-mingw32-gcc go test -race -count=1 ./...; \
	else \
		go test -count=1 ./...; \
	fi

# Sphinx docs (see docs/Makefile). uv provides sphinx + myst-parser + furo.
.PHONY: docs docs-html docs-dev docs-clean
docs docs-html:
	@$(MAKE) -C docs html
docs-dev:
	@$(MAKE) -C docs dev
docs-clean:
	@$(MAKE) -C docs clean

# RECIPES
$(BUILD)/docker-stamp: $(DOCKER_DEPS)
	@mkdir -p $(BUILD)
	@docker build --progress=plain -t $(IMAGE) . 2>&1 | tee $@

$(BUILD)/auth.json: scripts/codex-oauth.py
	@mkdir -p $(BUILD)
	@uv run scripts/codex-oauth.py > $@

$(BUILD)/pi-settings.json:
	@mkdir -p $(BUILD)
	@cat ~/.pi/agent/settings.json | jq '{ defaultProvider, defaultModel }' > $@
