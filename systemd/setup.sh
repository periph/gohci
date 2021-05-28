#!/bin/bash
# Copyright 2017 Marc-Antoine Ruel. All rights reserved.
# Use of this source code is governed under the Apache License, Version 2.0
# that can be found in the LICENSE file.

# Run as:
#   curl -sSL https://raw.githubusercontent.com/periph/gohci/master/systemd/setup.sh | bash
#
# For distros that require access to /dev/mem (anything but Raspbian), User= and
# Group= in gohci.service need to be changed to root.

set -eu

export GO111MODULE=on

if (which gohci-worker >/dev/null); then
  echo Found gohci-worker
else
  echo Fetching gohci-worker
  go install periph.io/x/gohci/cmd/gohci-worker@latest
fi

mkdir -p /home/${USER}/gohci


sudo tee /etc/systemd/system/gohci.service << EOF
# Created by https://github.com/periph/gohci/blob/master/systemd/setup.sh
[Unit]
Description=Go on Hardware CI
Wants=network-online.target
After=network-online.target
[Service]
User=${USER}
Group=${USER}
KillMode=mixed
Restart=always
TimeoutStopSec=20s
ExecStart=/home/${USER}/go/bin/gohci-worker
WorkingDirectory=/home/${USER}/gohci
Environment=PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
[Install]
WantedBy=default.target
EOF

sudo tee /etc/systemd/system/gohci_update.service << EOF
# Created by https://github.com/periph/gohci/blob/master/systemd/setup.sh
[Unit]
Description=Updates gohci-worker, as triggered by gohci_update.timer
After=network-online.target
[Service]
Type=oneshot
User=${USER}
Group=${USER}
NoNewPrivileges=true
# /bin/sh is necessary to load .profile to set $GOPATH:
ExecStart=/bin/sh -l -c "go install -v periph.io/x/gohci/cmd/gohci-worker@latest"
Environment=PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
Environment=GO111MODULE=on
EOF


sudo tee /etc/systemd/system/gohci_update.timer << EOF
# Created by https://github.com/periph/gohci/blob/master/systemd/setup.sh
[Unit]
Description=Runs "go get -u periph.io/x/gohci/cmd/gohci-worker" as a cron job
[Timer]
OnBootSec=1min
OnUnitActiveSec=1440min
RandomizedDelaySec=5
[Install]
WantedBy=timers.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable gohci_update.timer
sudo systemctl enable gohci.service
sudo systemctl start gohci.service
