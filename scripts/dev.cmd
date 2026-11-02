@echo off
rem Launcher for scripts\dev.ps1 so it runs without changing the machine
rem execution policy. Pass the same switches, e.g. dev.cmd -Check or dev.cmd -Stop.
setlocal
set "ROOT=%~dp0.."
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0dev.ps1" %*
exit /b %errorlevel%
