#!/bin/sh
# Copyright 2026 OverSide67
# SPDX-License-Identifier: Apache-2.0
# Запуск OffiChat (ПК-версия). Открывает http://localhost:8080
cd "$(dirname "$0")" || exit 1

if ! command -v go >/dev/null 2>&1; then
    if [ -x /usr/local/go/bin/go ]; then
        export PATH=$PATH:/usr/local/go/bin
    else
        echo "Go не найден. Сначала запустите: sh install.sh"
        exit 1
    fi
fi

exec go run .
