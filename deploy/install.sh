#!/bin/sh
# Installs or updates trains on the machine it runs on (as root):
#   ./install.sh path/to/trains-linux-amd64
# Works on systemd and OpenRC (Alpine) systems. Existing configuration and
# data are kept; the service is restarted with the new binary.
set -eu

bin=${1:?usage: install.sh BINARY}
here=$(dirname "$0")

install -m 0755 "$bin" /usr/local/bin/trains.new
mv /usr/local/bin/trains.new /usr/local/bin/trains

if ! id trains >/dev/null 2>&1; then
	if command -v adduser >/dev/null && [ -f /etc/alpine-release ]; then
		addgroup -S trains && adduser -S -D -H -G trains -s /sbin/nologin trains
	else
		useradd --system --home /var/lib/trains --shell /usr/sbin/nologin trains
	fi
fi
install -d -o trains -g trains -m 0750 /var/lib/trains
install -d -m 0755 /etc/trains
if [ ! -f /etc/trains/trains.yaml ]; then
	sed 's|^database: .*|database: /var/lib/trains/trains.db|' "$here/../config.example.yaml" > /etc/trains/trains.yaml
	echo "Wrote /etc/trains/trains.yaml: edit it, then start the service."
fi
if [ ! -f /etc/trains/env ]; then
	printf 'TELEGRAM_TOKEN=\nTELEGRAM_CHAT_ID=\n' > /etc/trains/env
	chmod 0600 /etc/trains/env
	echo "Wrote /etc/trains/env: put the Telegram token and chat id there."
fi

if command -v systemctl >/dev/null; then
	install -m 0644 "$here/trains.service" /etc/systemd/system/trains.service
	systemctl daemon-reload
	systemctl enable trains
	systemctl restart trains
elif command -v rc-service >/dev/null; then
	install -m 0755 "$here/trains.openrc" /etc/init.d/trains
	rc-update add trains default
	rc-service trains restart
else
	echo "No systemd or OpenRC found: start '/usr/local/bin/trains run -config /etc/trains/trains.yaml' yourself."
fi
/usr/local/bin/trains version
