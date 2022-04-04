#!/bin/sh
build_tags=""

if [[ $(arch) == "arm64" && -z "$LIBRARY_PATH" && -d /opt/homebrew ]]; then
	echo "Using /opt/homebrew for LIBRARY_PATH and CPATH"
	export LIBRARY_PATH=/opt/homebrew/lib
	export CPATH=/opt/homebrew/include
	HEIF_PATH="$LIBRARY_PATH"
else
	HEIF_PATH="/usr/local/lib"
fi

if [[ -f "$HEIF_PATH/libheif.1.dylib" ]]; then
	echo "libheif found in $HEIF_PATH, compiling with heif support"
	build_tags="libheif"
else
	echo "libheif not found in $HEIF_PATH, compiling without heif support"
fi

go build -tags "$build_tags" -ldflags "-X main.Tag=$(git describe --exact-match --tags 2>/dev/null) -X main.Commit=$(git rev-parse HEAD) -X 'main.BuildTime=`date '+%b %_d %Y, %H:%M:%S'`'"
