#!/bin/sh
exec "$CLANG" -v -arch $ARCH -target $ARCH-apple-ios -stdlib=libc++ -isysroot "$SDK_PATH" "$@"
