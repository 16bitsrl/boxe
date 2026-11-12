#!/bin/sh
# browse, with the extension this image installs in Chrome (not the Chrome
# Web Store's), at the version of the image: it does not update itself.
OPENCODE_BROWSER_EXTENSION_ORIGINS=chrome-extension://$(cat /usr/local/share/boxe/browse-extension/id)
OPENCODE_BROWSER_AUTO_UPDATE=off
export OPENCODE_BROWSER_EXTENSION_ORIGINS OPENCODE_BROWSER_AUTO_UPDATE
exec /usr/local/lib/boxe/browse "$@"
