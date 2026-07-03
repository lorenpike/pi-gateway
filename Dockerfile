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
    nginx \
    openssl \
    pandoc \
    poppler-utils \
    procps \
    python3 \
    ripgrep \
    software-properties-common \
    sqlite3 \
    sudo \
    supervisor \
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

RUN mkdir -p \
    /etc/supervisor/conf.d \
    /var/cache/nginx/client_temp \
    /var/cache/nginx/fastcgi_temp \
    /var/cache/nginx/proxy_temp \
    /var/cache/nginx/scgi_temp \
    /var/cache/nginx/uwsgi_temp \
    /var/log/wall-e

COPY static/etc/supervisor/supervisord.conf /etc/supervisor/supervisord.conf
COPY static/etc/supervisor/conf.d/ /etc/supervisor/conf.d/
COPY static/etc/nginx/nginx.conf /etc/nginx/nginx.conf

RUN deluser --remove-home ubuntu && \
    useradd -ms /bin/bash wall-e && \
    chown -R wall-e:wall-e /opt && \
    echo "wall-e ALL=(ALL) NOPASSWD:ALL" >> /etc/sudoers

USER wall-e
WORKDIR /home/wall-e

RUN mkdir -p \
    /home/wall-e/.config/supervisor.d \
    /home/wall-e/.config/nginx/conf.d \
    /home/wall-e/.config/nvim \
    /home/wall-e/sessions \
    /opt/pi \
    /opt/wall-e


COPY --from=build /usr/local/bin/wall-e /usr/local/bin/wall-e

COPY --chown=wall-e:wall-e --chmod=555 static/.vimrc static/.tmux.conf ./
COPY --chown=wall-e:wall-e --chmod=555 static/APPEND_SYSTEM.md /opt/pi
COPY --chown=wall-e:wall-e --chmod=555 static/CONTEXT.md /home/wall-e/CONTEXT.md
COPY --chown=wall-e:wall-e --chmod=555 static/SYSTEM.md /opt/wall-e/SYSTEM.md
COPY --chown=wall-e:wall-e --chmod=555 static/skills /opt/pi/skills
COPY --chown=wall-e:wall-e --chmod=555 static/www/ /opt/wall-e/www/

RUN ln -s /home/wall-e/.vimrc /home/wall-e/.config/nvim/init.vim

ENV HOME=/home/wall-e
ENV WALLE_SESSION_DIR=/home/wall-e/sessions
ENV WALLE_SITE=/opt/wall-e/www
ENV PI_CODING_AGENT_DIR=/opt/pi

USER root

ENTRYPOINT ["/usr/bin/tini", "--"]

CMD ["/usr/bin/supervisord", "-c", "/etc/supervisor/supervisord.conf"]
