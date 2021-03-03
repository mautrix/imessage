#!/bin/sh
set -x
SDK=iphoneos
SDK_PATH=`xcrun -sdk $SDK -show-sdk-path`
export IPHONEOS_DEPLOYMENT_TARGET=7.0
CLANG=`xcrun -sdk $SDK -find clang`

exec "$CLANG" -arch armv7 -target armv7-apple-ios -stdlib=libc++ -isysroot "$SDK_PATH" "$@"
