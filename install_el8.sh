#!/usr/bin/env bash
# Copyright 2014 Google Inc. All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# stenographer - full packet to disk capture
#
# stenographer is a simple, fast method of writing live packets to disk,
# then requesting those packets after-the-fact for post-hoc analysis.

#===============================================================#
# Installs Stenographer on Rocky Linux 8.6
#===============================================================#
# Note that some modifications are to allow modification to fapolicyd
# trusted files before starting stenographer.service

export KILLCMD=/usr/bin/pkill
export BINDIR="${BINDIR-/usr/bin}"
export GOPATH=${HOME}/go
export PATH=${PATH}:/usr/local/go/bin

# Get GID for stenographer
STENOGRAPHER_USER_GID="$(id -g stenographer)"

# Load support functions
_scriptDir="$(dirname $(readlink -f "$0"))"
# shellcheck source=lib.sh
source lib.sh

check_sudo() {
	Info "Checking for sudo...  "
	if (! sudo cat /dev/null); then
		Error "Failed. Please configure sudo support for this user."
		exit 1
	fi
}

stop_processes() {
	Info "Killing any already running processes..."
	sudo service stenographer stop
	ReallyKill stenographer
	ReallyKill stenotype
}

# For stenographer services and scripts to function correctly we will need to add a rule to whitelist needed directories in fapolicyd
# such that root (uid=0) can execute scripts in the listed directories and to allow execution of stenographer and stenotype

# Allow Install from $GOPATH:
allow perm=any uid=0 : dir="${GOPATH}"

# Allow $BINDIR/stenographer and $BINDIR/stenotype
allow perm=any uid="${STENOGRAPHER_USER_GID}" : dir="${BINDIR}"/stenographer,"${BINDIR}"/stenotype

install_packages() {
	Info "Installing stenographer package requirements...  "
	sudo dnf install -y epel-release
	sudo dnf makecache
	sudo dnf install -y libaio-devel leveldb-devel snappy-devel gcc-c++ make libcap-devel libseccomp-devel &>/dev/null

	if [ $? -ne 0 ]; then
		Error "Error. Please check that dnf can install needed packages."
		exit 2
	fi
}

install_golang() {
	if (! which go &>/dev/null); then
		Info "Installing golang ..."
		sudo dnf -y install golang
	fi
}

# Install jq, if not present
install_jq() {
	if (! which jq &>/dev/null); then
		Info "Installing jq ..."
		sudo dnf -y install jq
	fi
}

add_accounts() {
	if ! id stenographer &>/dev/null; then
		Info "Setting up stenographer user"
		sudo adduser --system --no-create-home stenographer
	fi
	if ! getent group stenographer &>/dev/null; then
		Info "Setting up stenographer group"
		sudo addgroup --system stenographer
	fi
}

install_configs() {
	cd "$_scriptDir" || exit

	Info "Setting up stenographer conf directory"
	if [ ! -d /etc/stenographer/certs ]; then
		sudo mkdir -p /etc/stenographer/certs
		sudo chown -R stenographer:stenographer /etc/stenographer/certs
	fi
	if [ ! -f /etc/stenographer/config ]; then
		sudo cp -vf configs/steno.conf /etc/stenographer/config
		sudo chown stenographer:stenographer /etc/stenographer/config
		sudo chmod 644 /etc/stenographer/config
	fi
	sudo chown stenographer:stenographer /etc/stenographer

	if grep -q /path/to /etc/stenographer/config; then
		Error "Create output directories for packets/index, then update"
		Error "/etc/stenographer/config"
		exit 1
	fi
}

install_certs() {
	cd "$_scriptDir" || exit
	sudo ./stenokeys.sh stenographer stenographer
}

install_service() {
	cd "$_scriptDir" || exit

	if [ ! -f /etc/security/limits.d/stenographer.conf ]; then
		Info "Setting up stenographer limits"
		sudo cp -v configs/limits.conf /etc/security/limits.d/stenographer.conf
	fi

	if [ ! -f /etc/systemd/system/stenographer.service ]; then
		Info "Installing stenographer systemd service"
		sudo cp -v configs/systemd.conf /etc/systemd/system/stenographer.service
		sudo chmod 0644 /etc/systemd/system/stenographer.service
	fi
}

build_stenographer() {

	if [ ! -x "$BINDIR/stenographer" ]; then
		Info "Building/Installing stenographer"
		/usr/local/go/bin/go get ./...
		/usr/local/go/bin/go build
		sudo cp -vf stenographer "$BINDIR/stenographer"
		sudo chown stenographer:root "$BINDIR/stenographer"
		sudo chmod 700 "$BINDIR/stenographer"
	else
		Info "stenographer already exists at $BINDIR/stenographer. Skipping"
	fi
}

build_stenotype() {
	cd "${_scriptDir}" || exit
	if [ ! -x "$BINDIR/stenotype" ]; then
		Info "Building/Installing stenotype"
		pushd "${_scriptDir}"/stenotype || exit
		make
		popd || exit
		sudo cp -vf stenotype/stenotype "$BINDIR/stenotype"
		sudo chown stenographer:root "$BINDIR/stenotype"
		sudo chmod 0500 "$BINDIR/stenotype"
		SetCapabilities "$BINDIR/stenotype"
	else
		Info "stenotype already exists at $BINDIR/stenotype. Skipping"
	fi
}

install_stenoread() {
	Info "Installing stenoread/stenocurl"
	sudo cp -vf stenoread "$BINDIR/stenoread"
	sudo chown root:root "$BINDIR/stenoread"
	sudo chmod 0755 "$BINDIR/stenoread"
	sudo cp -vf stenocurl "$BINDIR/stenocurl"
	sudo chown root:root "$BINDIR/stenocurl"
	sudo chmod 0755 "$BINDIR/stenocurl"
}

# TODO Insert routine to add stenographer and stenotype to fapolicyd trust
# systemctl stop fapolicyd
# fapolicyd-cli --file add /usr/bin/stenographer --trust-file mars-apps
# fapolicyd-cli --file add /usr/bin/stenotype --trust-file mars-apps
# touch /etc/fapolicyd/rules.d/80-mars-apps.rules
# allow perm=execute exe=/usr/bin/bash trust=1 : path=/usr/bin/stenographer ftype=application/x-executable trust=0
# allow perm=execute exe=/usr/bin/bash trust=1 : path=/usr/bin/stenotype ftype=application/x-executable trust=0
# More secure
# sha256sum /usr/bin/stenographer
# allow perm=execute exe=/usr/bin/bash trust=1 : sha256hash=780b75c90b2d41ea41679fcb358c892b1251b68d1927c80fbc0d9d148b25e836
# allow perm=execute exe=/usr/stenographer trust=1 : sha256hash=780b75c90b2d41ea41679fcb358c892b1251b68d1927c80fbc0d9d148b25e836
# fagenrules --check : If "Rules have changed and should be updated"
# fagenrules --load
# fapolicyd-cli --list
# Check output for allow and stenographer and stenotype files
# systemctl start fapolicyd

start_service() {
	Info "Starting stenographer service"
	sudo service stenographer start

	Info "Checking for running processes..."
	sleep 5
	if Running stenographer; then
		Info "  * Stenographer up and running"
	else
		Error "  !!! Stenographer not running !!!"
		sudo tail -n 100 /var/log/messages | grep steno
		exit 1
	fi
	if Running stenotype; then
		Info "  * Stenotype up and running"
	else
		Error "  !!! Stenotype not running !!!"
		sudo tail -n 100 /var/log/messages | grep steno
		exit 1
	fi
}

check_sudo
install_packages
install_golang
add_accounts
build_stenographer
build_stenotype
install_jq
install_configs
install_certs
install_service
install_stenoread
stop_processes
# Comment out actual start of service until fapolicyd trust mods
# configured to allow stenographer.service to run
# TODO Insert code to test for fapolicyd trust settings for stenographer and configure as needed
# start_service
