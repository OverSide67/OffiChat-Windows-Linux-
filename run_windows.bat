@echo off
rem Copyright 2026 OverSide67
rem SPDX-License-Identifier: Apache-2.0
rem Запуск OffiChat (ПК-версия). Открывает http://localhost:8080
cd /d "%~dp0"

where go >nul 2>nul
if %errorlevel% neq 0 (
    echo Go не найден. Сначала запустите install.bat
    pause
    exit /b 1
)

go run .
pause
