# laravel, an image for Laravel projects: boxeuntu plus PHP, Composer and Node.
# Build on the host, from a boxeuntu build (docker images boxeuntu):
#   docker build --build-arg BASE=boxeuntu:<version> -t laravel:<date> image/laravel
#   sudo scripts/build-base.sh laravel:<date> laravel-<date>
# then: ssh boxe new --image laravel
ARG BASE
FROM ${BASE}

# PHP 8.5 from Ondřej Surý's PPA: Ubuntu 24.04 only has 8.3. The build
# fails if the key served is not the PPA's signing key.
ARG PPA_KEY=B8DC7E53946656EFBCE4C1DD71DAEAAB4AD4CAB6
RUN curl -fsSL "https://keyserver.ubuntu.com/pks/lookup?op=get&search=0x${PPA_KEY}" | gpg --dearmor -o /usr/share/keyrings/ondrej-php.gpg && \
    gpg --show-keys --with-colons /usr/share/keyrings/ondrej-php.gpg | grep -qx "fpr:::::::::${PPA_KEY}:" && \
    echo "deb [signed-by=/usr/share/keyrings/ondrej-php.gpg] https://ppa.launchpadcontent.net/ondrej/php/ubuntu noble main" \
        > /etc/apt/sources.list.d/ondrej-php.list && \
    apt-get update && \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        php8.5-cli php8.5-bcmath php8.5-curl php8.5-gd php8.5-intl php8.5-mbstring php8.5-mysql \
        php8.5-pgsql php8.5-sqlite3 php8.5-xml php8.5-zip php8.5-redis unzip sqlite3 && \
    rm -rf /var/lib/apt/lists/*
# Composer and Node (for Vite, the current LTS), pinned like the base image.
ADD --checksum=sha256:9674f12a2a181100d889ebfd0b4510805b3e94cea57869b14b383753d602599b \
    https://getcomposer.org/download/2.11.0/composer.phar /usr/local/bin/composer
ADD --checksum=sha256:d28a96af6c38596f6d9d97c0ee531c13d238d757742387d1928de822d959e997 \
    https://nodejs.org/dist/v26.13.0/node-v26.13.0-linux-x64.tar.xz /tmp/node.tar.xz
RUN chmod 755 /usr/local/bin/composer && \
    tar -xJf /tmp/node.tar.xz -C /usr/local --strip-components=1 --exclude=CHANGELOG.md --exclude=LICENSE --exclude=README.md && \
    mkdir -p /usr/share/licenses/nodejs && \
    tar -xJf /tmp/node.tar.xz -C /usr/share/licenses/nodejs --strip-components=1 node-v26.13.0-linux-x64/LICENSE && \
    rm /tmp/node.tar.xz && php --version && composer --version && node --version
