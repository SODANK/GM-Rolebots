@echo off
rem Build the GuriMoon server WITH the rolebots module (no server repo edits).
rem   mod-rebuild          - build + replace server exe
rem   mod-rebuild check    - compile check only (exe goes to build\)
setlocal
set ARGS=
if /i "%1"=="check" set ARGS=-Check
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0mod-rebuild.ps1" %ARGS%
exit /b %ERRORLEVEL%
