# Copy this file to config.mk, then set your Station's IPv4 address.
# Device commands require root ADB already enabled on that Station.
STATION_IP =

# Optional startup values. Missing or exactly empty uses these defaults.
# Run make restart to apply changes; make start leaves a running client alone.
# No rebuild or new sign-in is needed. Names pass unchanged to ChatGPT.
# VOICE = orbit
# BACKEND_MODEL = gpt-6-thinking
# BACKEND_THINKING_EFFORT = extended
# Exactly disabled omits only the thinking-effort parameter.
# BACKEND_THINKING_EFFORT = disabled
# This is Make source. Use $$ for a dollar and \# for a literal hash.
# Quotes are data.

# make login opens installed Google Chrome. For Chromium or another Chrome
# location, give the program's full path. Spaces need no quotes.
# CHROME = /usr/bin/chromium
# GO = go
# UV = uv
# PYTHON = python3
# ADB = adb
# ARM Opus build jobs, from 1 to 32.
# JOBS = 2
