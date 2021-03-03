#!/bin/sh
exec "$CLANG" -v -arch armv7 -target armv7-apple-ios -stdlib=libc++ -isysroot "$SDK_PATH" "$@"
