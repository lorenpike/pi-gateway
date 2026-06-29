SPHINX_OPTS ?=
BUILDDIR ?= build
SOURCEDIR ?= source

.PHONY: help html dev clean

help:
	@echo "Targets:"
	@echo "  make html   Build the HTML docs"
	@echo "  make dev    Live-reload docs in the browser"
	@echo "  make clean  Remove build output"

html: $(BUILDDIR)/html/index.html
	@echo "Done!"

dev:
	uv run --with sphinx --with myst-parser --with furo --with sphinx-autobuild \
		python -m sphinx_autobuild $(SOURCEDIR) $(BUILDDIR)/html --open-browser $(SPHINX_OPTS)

clean:
	rm -rf $(BUILDDIR)

$(BUILDDIR)/html/index.html:
	uv run --with sphinx --with myst-parser --with furo \
		python -m sphinx -b html $(SOURCEDIR) $(BUILDDIR)/html $(SPHINX_OPTS)
