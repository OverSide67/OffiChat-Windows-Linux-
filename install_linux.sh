#!/bin/sh
# Copyright 2026 OverSide67
# SPDX-License-Identifier: Apache-2.0
# Установка Go и зависимостей для Bluetooth. Поддерживает apt, pacman, dnf, zypper.
# Если пакетного менеджера нет, ставится официальный архив с go.dev.
set -e

GO_VERSION="1.22.5"

detect_distro() {
    if [ -r /etc/os-release ]; then
        . /etc/os-release
        echo " ${ID:-unknown} ${ID_LIKE:-} "
    else
        echo " unknown "
    fi
}

DISTRO=$(detect_distro)
echo "Определена система:$DISTRO"

install_pkgs() {
    case "$DISTRO" in
        *" debian "*|*" ubuntu "*|*" linuxmint "*|*" pop "*)
            sudo apt-get update
            sudo apt-get install -y golang-go bluez libbluetooth-dev || true ;;
        *" arch "*|*" manjaro "*|*" endeavouros "*|*" cachyos "*)
            sudo pacman -Sy --needed --noconfirm go bluez bluez-utils || true ;;
        *" fedora "*|*" rhel "*|*" centos "*)
            sudo dnf install -y golang bluez bluez-libs-devel || true ;;
        *opensuse*|*" suse "*|*" sles "*)
            sudo zypper --non-interactive install go bluez bluez-devel || true ;;
        *)
            return 1 ;;
    esac
}

if command -v go >/dev/null 2>&1; then
    echo "Go уже установлен: $(go version)"
else
    if ! install_pkgs; then
        echo "Пакетный менеджер не распознан, ставлю Go с go.dev..."
        ARCH=$(uname -m)
        case "$ARCH" in
            x86_64) GOARCH=amd64 ;;
            aarch64|arm64) GOARCH=arm64 ;;
            *) echo "Неизвестная архитектура: $ARCH"; exit 1 ;;
        esac
        curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${GOARCH}.tar.gz" -o /tmp/go.tar.gz
        sudo rm -rf /usr/local/go
        sudo tar -C /usr/local -xzf /tmp/go.tar.gz
        echo 'export PATH=$PATH:/usr/local/go/bin' >> "$HOME/.profile"
        export PATH=$PATH:/usr/local/go/bin
    fi
fi

if command -v go >/dev/null 2>&1 || [ -x /usr/local/go/bin/go ]; then
    echo "Готово. Если команда go не находится в новом терминале, перезайдите."
else
    echo "Go не установлен."
    exit 1
fi
