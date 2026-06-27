BUILD := build
IMAGE := wall-e
STATIC_FILES := $(shell find static -type f)
DOCKER_DEPS := $(STATIC_FILES) Dockerfile $(BUILD)/pi-settings.json

# TARGETS
.PHONY: docker
docker: $(BUILD)/docker-stamp $(BUILD)/auth.json
	@BOT=wall-e; \
	 AUTH_FILE=/opt/pi/auth.json; \
	 SETTINGS_FILE=/opt/pi/settings.json; \
	 docker run -it --rm \
		--init \
		--name $(IMAGE) \
		-e OPENAI_API_KEY \
		-e OPENROUTER_API_KEY \
		-v "./$(BUILD)/auth.json:$$AUTH_FILE" \
		-v "./$(BUILD)/pi-settings.json:$$SETTINGS_FILE" \
		$(IMAGE) bash -lc "sudo chown -R $$BOT:$$BOT /opt/pi && exec tmux"


.PHONY: debug
debug:
	@echo $(DOCKER_DEPS)

# Run the Go test suite (optionally with -race via RACE=1).
# Uses the MinGW gcc for cgo when RACE=1, since Cygwin's gcc can't build
# native Windows programs.
.PHONY: test
test:
	@cd src && go vet ./... && \
		if [ "$$RACE" = "1" ]; then \
			CC=x86_64-w64-mingw32-gcc go test -race -count=1 ./...; \
		else \
			go test -count=1 ./...; \
		fi

$(BUILD)/docker-stamp: $(DOCKER_DEPS)
	@mkdir -p $(BUILD)
	@docker build --progress=plain -t $(IMAGE) . 2>&1 | tee $@

$(BUILD)/auth.json: scripts/codex-oauth.py
	@mkdir -p $(BUILD)
	@uv run scripts/codex-oauth.py > $@

$(BUILD)/pi-settings.json:
	@mkdir -p $(BUILD)
	@cat ~/.pi/agent/settings.json | jq '{ defaultProvider, defaultModel }' > $@
