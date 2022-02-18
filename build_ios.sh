#!/bin/bash

mkdir /tmp/arm64
curl -L "https://mau.dev/tulir/mautrix-imessage/-/jobs/artifacts/master/download?job=build%20ios%20arm64" -o /tmp/arm64/mautrix-imessage-arm64.zip
unzip /tmp/arm64/mautrix-imessage-arm64.zip -d /tmp/arm64
cp /tmp/arm64/mautrix-imessage-ios-arm64/libolm.3.dylib /tmp/arm64/mautrix-imessage-ios-arm64/libolm.dylib

export CGO_ENABLED=1
export GOARCH=arm64
export GOOS=ios
export IPHONEOS_DEPLOYMENT_TARGET="7.0"
export SDK=iphoneos
export ARCH=arm64
export SDK_PATH=$(xcrun -sdk $SDK -show-sdk-path)
export CLANG=$(xcrun -sdk $SDK -find clang)
export LIBRARY_PATH=/tmp/arm64/mautrix-imessage-ios-arm64
export CPATH=/opt/homebrew/include
export PATH=/opt/homebrew/bin:$PATH
export CC=$(pwd)/clangwrap.sh
export CXX=$CC
export GO_LDFLAGS="-X main.Tag=1.0.0 -X main.Commit=`git rev-parse --short HEAD` -X 'main.BuildTime=`date '+%b %_d %Y, %H:%M:%S'`'"

mkdir -p mautrix-imessage-ios-arm64
go build -tags ios -ldflags "$GO_LDFLAGS" -o mautrix-imessage-ios-arm64/mautrix-imessage
install_name_tool -change build/libolm.dylib.3.2.4 @executable_path/libolm.3.dylib mautrix-imessage-ios-arm64/mautrix-imessage
ldid -S mautrix-imessage-ios-arm64/mautrix-imessage
cp /tmp/arm64/mautrix-imessage-ios-arm64/libolm.dylib mautrix-imessage-ios-arm64/libolm.3.dylib
ldid -S mautrix-imessage-ios-arm64/libolm.3.dylib

rm -rf /tmp/arm64