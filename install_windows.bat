@echo off
rem Copyright 2026 OverSide67
rem SPDX-License-Identifier: Apache-2.0
rem Установка Go через winget, если его нет.
where go >nul 2>nul
if %errorlevel%==0 (
    echo Go уже установлен:
    go version
    pause
    exit /b 0
)

where winget >nul 2>nul
if %errorlevel% neq 0 (
    echo winget не найден. Скачайте MSI с https://go.dev/dl/ и запустите install.bat снова.
    pause
    exit /b 1
)

echo Устанавливаю Go через winget...
winget install --id GoLang.Go -e --accept-source-agreements --accept-package-agreements
if %errorlevel% neq 0 (
    echo Не удалось установить Go. Скачайте MSI вручную с https://go.dev/dl/
    pause
    exit /b 1
)

echo.
echo Готово. Закройте это окно и откройте новое, затем запустите run.bat.
pause
