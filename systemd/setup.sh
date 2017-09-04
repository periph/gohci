#!/bin/bash
# Copyright 2017 Marc-Antoine Ruel. All rights reserved.
# Use of this source code is governed under the Apache License, Version 2.0
# that can be found in the LICENSE file.

# Run as:
#   curl -sSL https://raw.githubusercontent.com/periph/gohci/master/systemd/setup.sh | bash


set -eu

if [ ! $# -eq 1 ]; then
  echo "Usage: $0 <suffix>"
  echo ""
  echo " suffix should be something like '_project'"
  exit 2
fi

SUFFIX=$1


mkdir /home/${USER}/gohci${SUFFIX}


sudo tee /etc/systemd/system/gohci${SUFFIX}.service << EOF
# Created by https://github.com/periph/gohci/blob/master/systemd/setup.sh
[Unit]
Description=Go on Hardware CI
Wants=network-online.target
After=network-online.target

[Service]
User=${USER}
Group=${USER}
# Grant unconditional access to physical memory. This is as good as giving root
# but this makes file ownership simpler.
PermissionsStartOnly=true
ExecStartPre=chown root.${USER} /dev/mem && chmod g+rw /dev/mem
KillMode=mixed
Restart=always
TimeoutStopSec=20s
ExecStart=/home/${USER}/go/bin/gohci
WorkingDirectory=/home/${USER}/gohci${SUFFIX}
# To use port 80 not as root:
#   Systemd 229:
#     AmbientCapabilities=CAP_NET_BIND_SERVICE
#   Systemd 228 and below:
#     SecureBits=keep-caps
#     Capabilities=cap_net_bind_service+pie
# - Normal installation of a recent Go version in /usr/local/go:
Environment=PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

[Install]
WantedBy=default.target
EOF

sudo tee /etc/systemd/system/gohci_update.service << EOF
# Created by https://github.com/periph/gohci/blob/master/systemd/setup.sh
[Unit]
Description=Updates gohci, as triggered by gohci_update.timer
After=network-online.target
[Service]
Type=oneshot
User=${USER}
Group=${USER}
NoNewPrivileges=true
# /bin/sh is necessary to load .profile to set $GOPATH:
ExecStart=/bin/sh -l -c "go get -v -u periph.io/x/gohci"
Environment=PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
EOF


sudo tee /etc/systemd/system/gohci_update.timer << EOF
# Created by https://github.com/periph/gohci/blob/master/systemd/setup.sh
[Unit]
Description=Runs "go get -u periph.io/x/gohci" as a cron job
[Timer]
OnBootSec=1min
OnUnitActiveSec=10min
RandomizedDelaySec=5
[Install]
WantedBy=timers.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable gohci_update.timer
sudo systemctl start gohci_update.timer
sudo systemctl enable gohci${SUFFIX}.service
sudo systemctl start gohci${SUFFIX}.service
