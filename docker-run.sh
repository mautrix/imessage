#!/bin/sh

if [[ -z "$GID" ]]; then
	GID="$UID"
fi

if [[ ! -f /data/config.yaml ]]; then
	cp /opt/mautrix-imessage/example-config.yaml /data/config.yaml
	echo "Didn't find a config file."
	echo "Copied default config file to /data/config.yaml"
	echo "Modify that config file to your liking."
	echo "Start the container again after that to generate the registration file."
	exit
fi

if [[ ! -f /data/registration.yaml ]]; then
	/usr/bin/mautrix-imessage -g -c /data/config.yaml -r /data/registration.yaml || exit $?
	echo "Didn't find a registration file."
	echo "Generated one for you."
	echo "See https://docs.mau.fi/bridges/general/registering-appservices.html on how to use it."
	exit
fi

cd /data

chown -R $UID:$GID /data
#exec su-exec $UID:$GID /usr/bin/mautrix-imessage
exec /usr/bin/mautrix-imessage
