FROM golang:1.26 AS build
# Static binary; no cgo (stdlib-only, no platform libc tie-in).
COPY src/ src/
RUN CGO_ENABLED=0 GOOS=linux cd src && go build -trimpath -o /usr/local/bin/wall-e .

FROM ubuntu:24.04

RUN apt update && apt install -y \
    build-essential \
    ca-certificates \
    curl \
    fd-find \
    gh \
    git \
    gnupg \
    golang-go \
    jq \
    locales \
    lsb-release \
    lsof \
    ncurses-term \
    openssl \
    pandoc \
    poppler-utils \
    procps \
    python3 \
    ripgrep \
    software-properties-common \
    sqlite3 \
    sudo \
    tmux \
    unzip \
    zip \
    && rm -rf /var/lib/apt/lists/*

RUN apt-get update && apt-get install -y --no-install-recommends tini \
    && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL https://deb.nodesource.com/setup_24.x | bash - \
    && apt update && apt install -y nodejs \
    && rm -rf /var/lib/apt/lists/*

RUN add-apt-repository ppa:neovim-ppa/stable -y \
    && apt update && apt install -y neovim \
    && rm -rf /var/lib/apt/lists/*

RUN npm install -g --ignore-scripts @earendil-works/pi-coding-agent

RUN ln -s "$(which fdfind)" /usr/local/bin/fd && \
    ln -s "$(which nvim)" /usr/local/bin/vi

RUN deluser --remove-home ubuntu && \
    useradd -ms /bin/bash wall-e && \
    chown -R wall-e:wall-e /home/wall-e && \
    mkdir -p /opt/pi /opt/wall-e && chown -R wall-e:wall-e /opt/pi /opt/wall-e && \
    mkdir -p /home/wall-e/sessions && \
    chown -R wall-e:wall-e /home/wall-e/sessions && \
    echo "wall-e ALL=(ALL) NOPASSWD:ALL" >> /etc/sudoers

COPY --from=build /usr/local/bin/wall-e /usr/local/bin/wall-e
COPY --chown=wall-e:wall-e static/SYSTEM.md /opt/wall-e/SYSTEM.md
COPY --chown=wall-e:wall-e static/CONTEXT.md /home/wall-e/CONTEXT.md

USER wall-e
WORKDIR /home/wall-e

COPY --chown=wall-e:wall-e static/.vimrc static/.tmux.conf ./
RUN mkdir -p .config/nvim && ln -s /home/wall-e/.vimrc .config/nvim/init.vim

ENV WALLE_SESSION_DIR=/home/wall-e/sessions
ENV PI_CODING_AGENT_DIR=/opt/pi

COPY --chown=wall-e:wall-e static/APPEND_SYSTEM.md /opt/pi
COPY --chown=wall-e:wall-e static/skills /opt/pi/skills


ENTRYPOINT ["/usr/bin/tini", "--"]

CMD ["/usr/local/bin/wall-e"]
