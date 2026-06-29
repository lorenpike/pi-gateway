from pathlib import Path

project = "wall-e"
author = "Metrized"
root = Path(__file__).resolve().parent

extensions = [
    "myst_parser",
    "sphinx.ext.autosectionlabel",
]

exclude_patterns = ["build", "Thumbs.db", ".DS_Store"]
myst_enable_extensions = [
    "colon_fence",
    "deflist",
    "fieldlist",
]
autosectionlabel_prefix_document = True
html_theme = "furo"
html_title = "wall-e"
html_static_path = ["_static"]
