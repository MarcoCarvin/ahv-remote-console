FROM node:24-bookworm

# Wine + 32-bit support + libssl1.1 (electron-builder ships
# winCodeSign-2.6.0 linked against OpenSSL 1.1 which is unavailable
# in Debian bookworm by default). Pull libssl1.1 from the bullseye
# pocket, then drop the pocket so apt isn't permanently extended.
RUN dpkg --add-architecture i386 \
    && echo "deb http://deb.debian.org/debian bullseye main" > /etc/apt/sources.list.d/bullseye.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
       wine wine64 wine32 \
       rpm fakeroot \
       libssl1.1 \
    && rm /etc/apt/sources.list.d/bullseye.list \
    && apt-get clean && rm -rf /var/lib/apt/lists/* \
    && corepack enable \
    && COREPACK_INTEGRITY_KEYS=0 corepack prepare pnpm@11.1.3 --activate

ENV COREPACK_INTEGRITY_KEYS=0
ENV WINEDEBUG=-all
