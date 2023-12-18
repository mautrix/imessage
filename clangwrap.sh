#!/bin/sh

# Check if the required variables are set
if [ -z "$CLANG" ]; then
    echo "Error: CLANG variable is not set."
    exit 1
fi

if [ -z "$ARCH" ]; then
    echo "Error: ARCH (architecture) variable is not set."
    exit 1
fi

if [ -z "$SDK_PATH" ]; then
    echo "Error: SDK_PATH variable is not set."
    exit 1
fi

# Execute Clang with the provided arguments
echo "Executing Clang with architecture: $ARCH"
exec "$CLANG" -v -arch "$ARCH" -target "$ARCH"-apple-ios -stdlib=libc++ -isysroot "$SDK_PATH" "$@"
