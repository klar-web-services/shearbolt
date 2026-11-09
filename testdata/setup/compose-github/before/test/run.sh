#!/bin/sh
set -e
docker compose -f test/compose.yml up -d --wait
