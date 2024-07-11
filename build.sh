#!/bin/bash
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
	build_tags="libheif"
fi

go build -o mautrix-imessage -tags "$build_tags" -ldflags "-X main.Tag=$(git describe --exact-match --tags 2>/dev/null) -X main.Commit=$(git rev-parse HEAD) -X 'main.BuildTime=`date '+%b %_d %Y, %H:%M:%S'`'" "$@"
