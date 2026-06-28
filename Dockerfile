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

ARG USERNAME=wall-e
RUN deluser --remove-home ubuntu && \
    useradd -ms /bin/bash $USERNAME && \
    chown -R $USERNAME:$USERNAME /home/$USERNAME && \
    mkdir -p /opt/pi && chown -R $USERNAME:$USERNAME /opt/pi && \
    mkdir -p /home/$USERNAME/sessions && \
    chown -R $USERNAME:$USERNAME /home/$USERNAME/sessions && \
    echo "$USERNAME ALL=(ALL) NOPASSWD:ALL" >> /etc/sudoers

COPY --from=build /usr/local/bin/wall-e /usr/local/bin/wall-e

USER $USERNAME
WORKDIR /home/$USERNAME

COPY static/.vimrc static/.tmux.conf ./
RUN mkdir -p .config/nvim && cp .vimrc .config/nvim/init.vim

ENV WALLE_SESSION_DIR=/home/wall-e/sessions
ENV PI_CODING_AGENT_DIR=/opt/pi
COPY static/APPEND_SYSTEM.md /opt/pi


ENTRYPOINT ["/usr/bin/tini", "--"]

CMD ["/usr/local/bin/wall-e"]
